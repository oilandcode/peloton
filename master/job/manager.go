package job

import (
	"github.com/yarpc/yarpc-go"
	"github.com/yarpc/yarpc-go/encoding/json"
	"golang.org/x/net/context"

	"code.uber.internal/go-common.git/x/log"
	"code.uber.internal/infra/peloton/storage"
	"code.uber.internal/infra/peloton/util"
	"fmt"
	"github.com/pborman/uuid"
	mesos_v1 "mesos/v1"
	"peloton/job"
	"peloton/master/taskqueue"
	"peloton/task"
	"time"
)

func InitManager(d yarpc.Dispatcher, store storage.JobStore, taskStore storage.TaskStore) {
	handler := jobManager{
		JobStore:  store,
		TaskStore: taskStore,
		client:    json.New(d.Channel("peloton-master")),
		rootCtx:   context.Background(),
	}
	json.Register(d, json.Procedure("JobManager.Create", handler.Create))
	json.Register(d, json.Procedure("JobManager.Get", handler.Get))
	json.Register(d, json.Procedure("JobManager.Query", handler.Query))
	json.Register(d, json.Procedure("JobManager.Delete", handler.Delete))
}

type jobManager struct {
	JobStore  storage.JobStore
	TaskStore storage.TaskStore
	TaskQueue util.TaskQueue
	client    json.Client
	rootCtx   context.Context
}

func (m *jobManager) Create(
	ctx context.Context,
	reqMeta yarpc.ReqMeta,
	body *job.CreateRequest) (*job.CreateResponse, yarpc.ResMeta, error) {

	jobId := body.Id
	jobConfig := body.Config
	
	log.WithField("config", jobConfig).Info("Creating job with config")
	err := m.JobStore.CreateJob(jobId, jobConfig, "peloton")
	if err != nil {
		return &job.CreateResponse{
			AlreadyExists: &job.JobAlreadyExists{
				Id:      body.Id,
				Message: err.Error(),
			},
		}, nil, nil
	}
	// Create tasks for the job
	for i := 0; i < int(body.Config.InstanceCount); i++ {
		taskId := fmt.Sprintf("%s-%d-%v", jobId.Value, i, uuid.NewUUID().String())
		taskInfo := task.TaskInfo{
			Runtime: &task.RuntimeInfo{
				State: task.RuntimeInfo_INITIALIZED,
				TaskId: &mesos_v1.TaskID{
					Value: &taskId,
				},
			},
			JobConfig:  jobConfig,
			InstanceId: uint32(i),
			JobId:      jobId,
		}
		log.Debugf("Creating %v =th task for job %v", i, jobId)
		err := m.TaskStore.CreateTask(jobId, i, &taskInfo, "peloton")
		if err != nil {
			log.Errorf("Creating %v =th task for job %v failed with err=%v", i, jobId, err)
			continue
			// TODO : decide how to handle the case that some tasks
			// failed to be added (rare)

			// 1. Rely on job level healthcheck to alert on # of
			// instances mismatch, and re-try creating the task later
			
			// 2. revert te job creation altogether
		}
		// Put the task into the taskQueue. Scheduler will pick the
		// task up and schedule them
		// TODO: batch the tasks for each Enqueue request
		m.putTasks([]*task.TaskInfo{&taskInfo})
	}
	return &job.CreateResponse{
		Result: jobId,
	}, nil, nil
}

func (m *jobManager) Get(
	ctx context.Context,
	reqMeta yarpc.ReqMeta,
	body *job.GetRequest) (*job.GetResponse, yarpc.ResMeta, error) {
	log.Infof("JobManager.Get called: %v", body)

	jobConfig, err := m.JobStore.GetJob(body.Id)
	if err != nil {
		log.Errorf("GetJob failed with error %v", err)
	}
	return &job.GetResponse{Result: jobConfig}, nil, nil
}

func (m *jobManager) Query(
	ctx context.Context,
	reqMeta yarpc.ReqMeta,
	body *job.QueryRequest) (*job.QueryResponse, yarpc.ResMeta, error) {
	log.Infof("JobManager.Query called: %v", body)

	jobConfigs, err := m.JobStore.Query(body.Labels)
	if err != nil {
		log.Errorf("Query job failed with error %v", err)
	}
	return &job.QueryResponse{Result: jobConfigs}, nil, nil
}

func (m *jobManager) Delete(
	ctx context.Context,
	reqMeta yarpc.ReqMeta,
	body *job.DeleteRequest) (*job.DeleteResponse, yarpc.ResMeta, error) {

	log.Infof("JobManager.Delete called: %v", body)
	err := m.JobStore.DeleteJob(body.Id)
	if err != nil {
		log.Errorf("Delete job failed with error %v", err)
	}
	return &job.DeleteResponse{}, nil, nil
}

func (m *jobManager) putTasks(tasks []*task.TaskInfo) error {
	ctx, _ := context.WithTimeout(m.rootCtx, 10*time.Second)
	var response taskqueue.EnqueueResponse
	var request = &taskqueue.EnqueueRequest{
		Tasks: tasks,
	}
	_, err := m.client.Call(
		ctx,
		yarpc.NewReqMeta().Procedure("TaskQueue.Enqueue"),
		request,
		&response,
	)
	if err != nil {
		log.Errorf("Deque failed with err=%v", err)
		return err
	}
	log.Debugf("Enqueued %d tasks to leader", len(tasks))
	return nil
}
