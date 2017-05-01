package job

import (
	"fmt"
	"time"

	"peloton/api/job"
	"peloton/api/peloton"
	"peloton/api/task"

	mesos "mesos/v1"

	jm_task "code.uber.internal/infra/peloton/jobmgr/task"
	"code.uber.internal/infra/peloton/storage"
	"code.uber.internal/infra/peloton/util"
	log "github.com/Sirupsen/logrus"
	"github.com/pborman/uuid"
	"github.com/uber-go/tally"
	"go.uber.org/yarpc/encoding/json"
)

const (
	batchRows        = uint32(1000)
	recoveryInterval = 15 * time.Minute
	// Use the same layout string in time.Time.String().
	// See https://golang.org/pkg/time/#String
	timeLayout = "2006-01-02 15:04:05.999999999 -0700 MST"
)

// Recovery scans jobs and make sure that all tasks are created and
// properly sent to RM, when a job manager becomes leader. A job can have its tasks
// partially created, as the job manager instance can fail
type Recovery struct {
	jobStore         storage.JobStore
	taskStore        storage.TaskStore
	resmgrClient     json.Client
	lastRecoveryTime time.Time
	metrics          *RecoveryMetrics
}

// NewJobRecovery creates a JobStateValidator
func NewJobRecovery(
	jobStore storage.JobStore,
	taskStore storage.TaskStore,
	resmgrClient json.Client,
	parentScope tally.Scope) *Recovery {

	return &Recovery{
		jobStore:     jobStore,
		taskStore:    taskStore,
		resmgrClient: resmgrClient,
		metrics:      NewRecoveryMetrics(parentScope.SubScope("job_recovery")),
	}
}

// recoverJobs validates all jobs to make sure that all tasks
// are created and sent to RM, for jobs in INITIALIZED state
func (j *Recovery) recoverJobs() {
	if time.Since(j.lastRecoveryTime) < recoveryInterval {
		return
	}
	j.lastRecoveryTime = time.Now()
	jobStates := []job.JobState{
		job.JobState_INITIALIZED,
	}
	for _, state := range jobStates {
		jobIDs, err := j.jobStore.GetJobsByState(state)
		if err != nil {
			log.WithError(err).
				WithField("state", state).
				Error("Failed to GetJobsByState")
			continue
		}
		for _, jobID := range jobIDs {
			err := j.recoverJob(&jobID)
			if err == nil {
				j.metrics.JobRecovered.Inc(1)
			} else {
				j.metrics.JobRecoverFailed.Inc(1)
			}
		}
	}
}

// Make sure that all tasks created and queued to RM
func (j *Recovery) recoverJob(jobID *peloton.JobID) error {
	log.WithField("job_id", jobID.Value).Info("recovering job")

	jobConfig, err := j.jobStore.GetJobConfig(jobID)
	if err != nil {
		log.WithError(err).
			WithField("job_id", jobID).
			Error("Failed to get jobConfig")
		return err
	}

	jobRuntime, err := j.jobStore.GetJobRuntime(jobID)
	if err != nil {
		log.WithError(err).
			WithField("job_id", jobID).
			Error("Failed to GetJobRuntime")
		return err
	}
	// Using the same layout string as used in time.Time.String()
	// see https://golang.org/pkg/time/#String
	createTime, err := time.Parse(timeLayout, jobRuntime.CreationTime)
	if err != nil {
		log.WithError(err).
			WithField("job_id", jobID).
			WithField("create_time", jobRuntime.CreationTime).
			Error("Failed to Parse job create time")
		return err
	}
	// Only recover job that still in Initialized state after recoveryInterval
	// this is for avoiding collision with jobs being created right now
	if time.Since(createTime) < recoveryInterval {
		log.WithField("job_id", jobID).
			WithField("create_time", createTime).
			Info("Job created recently, skip")
		return nil
	}

	for batch := uint32(0); batch < jobConfig.InstanceCount/batchRows+1; batch++ {
		var tasksToRequeue []*task.TaskInfo
		start := batch * batchRows
		end := util.Min((batch+1)*batchRows, jobConfig.InstanceCount)
		log.WithField("start", start).
			WithField("end", end).
			Debug("Validating task range")
		for i := start; i < end; i++ {
			t, err := j.taskStore.GetTaskForJob(jobID, i)
			if err != nil {
				_, ok := err.(*storage.TaskNotFoundError)
				if !ok {
					log.WithError(err).
						WithField("job_id", jobID.Value).
						WithField("id", i).
						Error("Failed to GetTaskForJob")
					continue
				}
				// Task does not exist in DB
				log.WithField("job_id", jobID.Value).
					WithField("task_instance", i).
					Info("Creating missing task")
				task, err := createTaskForJob(j.taskStore, jobID, i, jobConfig)
				if err != nil {
					log.WithError(err).
						WithField("job_id", jobID.Value).
						WithField("id", i).
						Error("Failed to CreateTask")
					j.metrics.TaskRecoverFailed.Inc(1)
					return err
				}
				j.metrics.TaskRecovered.Inc(1)
				tasksToRequeue = append(tasksToRequeue, task)
			} else {
				//Task exists, check if task is in initialized state, if yes then requeue to RM
				var taskState task.TaskState
				for _, taskInfo := range t {
					taskState = taskInfo.Runtime.State
					// Task state in initialized need to be requeued to RM
					if taskState == task.TaskState_INITIALIZED {
						tasksToRequeue = append(tasksToRequeue, taskInfo)
						log.WithField("job_id", jobID.Value).
							WithField("task_instance", i).
							Info("Requeue initialized task")
					}
				}
			}
		}

		if len(tasksToRequeue) > 0 {
			// requeue the tasks into resgmr
			// TODO: retry policy
			err := EnqueueTasks(tasksToRequeue, jobConfig, j.resmgrClient)
			if err != nil {
				log.WithError(err).
					WithField("job_id", jobID.Value).
					Error("Failed to EnqueueTasks to resmgr")
				j.metrics.TaskRequeueFailed.Inc(int64(len(tasksToRequeue)))
				return err
			}
			j.metrics.TaskRequeued.Inc(int64(len(tasksToRequeue)))
		}
	}
	jobRuntime.State = job.JobState_PENDING
	err = j.jobStore.UpdateJobRuntime(jobID, jobRuntime)
	if err != nil {
		log.WithError(err).
			WithField("job_id", jobID).
			Error("Failed to UpdateJobRuntime")
		return err
	}
	return nil
}

func createTaskForJob(
	taskStore storage.TaskStore,
	jobID *peloton.JobID,
	i uint32,
	jobConfig *job.JobConfig) (*task.TaskInfo, error) {
	mesosTaskID := fmt.Sprintf("%s-%d-%s", jobID.Value, i,
		uuid.NewUUID().String())
	taskConfig, _ := jm_task.GetTaskConfig(jobConfig, i)
	task := task.TaskInfo{
		Runtime: &task.RuntimeInfo{
			State: task.TaskState_INITIALIZED,
			// New task is by default treated as batch task and get SUCCEEDED goalstate.
			// TODO(mu): Long running tasks need RUNNING as default goalstate.
			GoalState: task.TaskState_SUCCEEDED,
			TaskId: &mesos.TaskID{
				Value: &mesosTaskID,
			},
		},
		Config:     taskConfig,
		InstanceId: i,
		JobId:      jobID,
	}
	err := taskStore.CreateTask(jobID, i, &task, jobConfig.OwningTeam)
	if err != nil {
		log.WithError(err).
			WithField("job_id", jobID.Value).
			WithField("id", i).
			Error("Failed to CreateTask")
		return nil, err
	}
	return &task, nil
}