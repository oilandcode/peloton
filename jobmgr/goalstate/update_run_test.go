package goalstate

import (
	"context"
	"fmt"
	"testing"
	"time"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	pbjob "code.uber.internal/infra/peloton/.gen/peloton/api/v0/job"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	pbtask "code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"
	pbupdate "code.uber.internal/infra/peloton/.gen/peloton/api/v0/update"
	"code.uber.internal/infra/peloton/.gen/peloton/private/models"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"
	resmocks "code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc/mocks"

	"code.uber.internal/infra/peloton/common/goalstate"
	goalstatemocks "code.uber.internal/infra/peloton/common/goalstate/mocks"
	"code.uber.internal/infra/peloton/jobmgr/cached"
	cachedmocks "code.uber.internal/infra/peloton/jobmgr/cached/mocks"
	jobmgrcommon "code.uber.internal/infra/peloton/jobmgr/common"
	goalstateutil "code.uber.internal/infra/peloton/jobmgr/util/goalstate"
	storemocks "code.uber.internal/infra/peloton/storage/mocks"

	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"
	"go.uber.org/yarpc/yarpcerrors"
)

type UpdateRunTestSuite struct {
	suite.Suite
	ctrl                  *gomock.Controller
	updateFactory         *cachedmocks.MockUpdateFactory
	jobFactory            *cachedmocks.MockJobFactory
	updateGoalStateEngine *goalstatemocks.MockEngine
	taskGoalStateEngine   *goalstatemocks.MockEngine
	goalStateDriver       *driver
	jobID                 *peloton.JobID
	updateID              *peloton.UpdateID
	updateEnt             *updateEntity
	cachedJob             *cachedmocks.MockJob
	cachedUpdate          *cachedmocks.MockUpdate
	cachedTask            *cachedmocks.MockTask
	jobStore              *storemocks.MockJobStore
	taskStore             *storemocks.MockTaskStore
	resmgrClient          *resmocks.MockResourceManagerServiceYARPCClient
}

func TestUpdateRun(t *testing.T) {
	suite.Run(t, new(UpdateRunTestSuite))
}

func (suite *UpdateRunTestSuite) SetupTest() {
	suite.ctrl = gomock.NewController(suite.T())
	suite.updateFactory = cachedmocks.NewMockUpdateFactory(suite.ctrl)
	suite.jobFactory = cachedmocks.NewMockJobFactory(suite.ctrl)
	suite.updateGoalStateEngine = goalstatemocks.NewMockEngine(suite.ctrl)
	suite.taskGoalStateEngine = goalstatemocks.NewMockEngine(suite.ctrl)
	suite.jobStore = storemocks.NewMockJobStore(suite.ctrl)
	suite.taskStore = storemocks.NewMockTaskStore(suite.ctrl)
	suite.resmgrClient = resmocks.NewMockResourceManagerServiceYARPCClient(suite.ctrl)
	suite.goalStateDriver = &driver{
		updateFactory: suite.updateFactory,
		jobFactory:    suite.jobFactory,
		updateEngine:  suite.updateGoalStateEngine,
		taskEngine:    suite.taskGoalStateEngine,
		jobStore:      suite.jobStore,
		taskStore:     suite.taskStore,
		mtx:           NewMetrics(tally.NoopScope),
		cfg:           &Config{},
		resmgrClient:  suite.resmgrClient,
	}
	suite.goalStateDriver.cfg.normalize()

	suite.jobID = &peloton.JobID{Value: uuid.NewRandom().String()}
	suite.updateID = &peloton.UpdateID{Value: uuid.NewRandom().String()}
	suite.updateEnt = &updateEntity{
		id:     suite.updateID,
		jobID:  suite.jobID,
		driver: suite.goalStateDriver,
	}

	suite.cachedJob = cachedmocks.NewMockJob(suite.ctrl)
	suite.cachedUpdate = cachedmocks.NewMockUpdate(suite.ctrl)
	suite.cachedTask = cachedmocks.NewMockTask(suite.ctrl)
}

func (suite *UpdateRunTestSuite) TearDownTest() {
	suite.ctrl.Finish()
}

func (suite *UpdateRunTestSuite) TestRunningUpdate() {
	instancesTotal := []uint32{2, 3, 4, 5, 6}
	oldJobConfigVer := uint64(3)
	newJobConfigVer := uint64(4)

	updateConfig := &pbupdate.UpdateConfig{
		BatchSize: 0,
	}

	runtimeDone := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_RUNNING,
		GoalState:            pbtask.TaskState_RUNNING,
		Healthy:              pbtask.HealthState_HEALTHY,
		ConfigVersion:        newJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	runtimeRunning := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_RUNNING,
		GoalState:            pbtask.TaskState_RUNNING,
		Healthy:              pbtask.HealthState_HEALTHY,
		ConfigVersion:        oldJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	runtimeTerminated := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_KILLED,
		GoalState:            pbtask.TaskState_KILLED,
		Healthy:              pbtask.HealthState_INVALID,
		ConfigVersion:        oldJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	runtimeInitialized := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_INITIALIZED,
		GoalState:            pbtask.TaskState_RUNNING,
		Healthy:              pbtask.HealthState_HEALTH_UNKNOWN,
		ConfigVersion:        newJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	runtimenNotReady := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_RUNNING,
		GoalState:            pbtask.TaskState_RUNNING,
		Healthy:              pbtask.HealthState_HEALTH_UNKNOWN,
		ConfigVersion:        newJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  instancesTotal,
			JobVersion: uint64(4),
		}).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(instancesTotal)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(nil).Times(2)

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(instancesTotal)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(updateConfig).
		Times(3)

	for _, instID := range instancesTotal {
		suite.cachedJob.EXPECT().
			AddTask(gomock.Any(), instID).
			Return(suite.cachedTask, nil)

		if instID == instancesTotal[0] {
			// only 1 of the task is still running
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeRunning, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeRunning).
				Return(false)
			suite.cachedUpdate.EXPECT().
				IsInstanceFailed(runtimeRunning, updateConfig.GetMaxInstanceAttempts()).
				Return(false)
			suite.cachedUpdate.EXPECT().
				IsInstanceInProgress(newJobConfigVer, runtimeRunning).
				Return(true)
		} else if instID == instancesTotal[1] {
			// only 1 task is in terminal goal state
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeTerminated, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeTerminated).
				Return(true)
		} else if instID == instancesTotal[2] {
			// only 1 task is in initialized state
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeInitialized, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeInitialized).
				Return(false)
			suite.cachedUpdate.EXPECT().
				IsInstanceFailed(runtimeInitialized, updateConfig.GetMaxInstanceAttempts()).
				Return(false)
			suite.cachedUpdate.EXPECT().
				IsInstanceInProgress(newJobConfigVer, runtimeInitialized).
				Return(true)
		} else if instID == instancesTotal[3] {
			// only 1 task is in initialized state
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimenNotReady, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimenNotReady).
				Return(false)
			suite.cachedUpdate.EXPECT().
				IsInstanceFailed(runtimenNotReady, updateConfig.GetMaxInstanceAttempts()).
				Return(false)
			suite.cachedUpdate.EXPECT().
				IsInstanceInProgress(newJobConfigVer, runtimenNotReady).
				Return(true)
		} else {
			// rest are updated
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeDone, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeDone).
				Return(true)
		}
	}

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		})

	suite.cachedUpdate.EXPECT().
		WriteProgress(
			gomock.Any(),
			pbupdate.State_ROLLING_FORWARD,
			[]uint32{3, 6},
			[]uint32{},
			[]uint32{2, 4, 5},
		).Return(nil)

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

func (suite *UpdateRunTestSuite) TestCompletedUpdate() {
	desiredConfigVersion := uint64(3)
	var instancesRemaining []uint32
	instancesTotal := []uint32{2, 3, 4, 5}
	runtime := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_RUNNING,
		Healthy:              pbtask.HealthState_HEALTHY,
		GoalState:            pbtask.TaskState_RUNNING,
		ConfigVersion:        desiredConfigVersion,
		DesiredConfigVersion: desiredConfigVersion,
	}

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(instancesTotal)

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(instancesTotal)

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  instancesTotal,
			JobVersion: uint64(3),
		}).
		Times(2)

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return(instancesRemaining)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(&pbupdate.UpdateConfig{
			BatchSize: 0,
		}).
		Times(3)

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID)

	for _, instID := range instancesTotal {
		suite.cachedJob.EXPECT().
			AddTask(gomock.Any(), instID).
			Return(suite.cachedTask, nil)

		suite.cachedTask.EXPECT().
			GetRunTime(gomock.Any()).
			Return(runtime, nil)

		suite.cachedUpdate.EXPECT().
			IsInstanceComplete(desiredConfigVersion, runtime).
			Return(true)
	}

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		})

	suite.cachedUpdate.EXPECT().
		WriteProgress(
			gomock.Any(),
			pbupdate.State_ROLLING_FORWARD,
			[]uint32{2, 3, 4, 5},
			instancesRemaining,
			instancesRemaining,
		).Return(nil)

	suite.cachedUpdate.EXPECT().
		ID().
		Return(suite.updateID)

	suite.updateGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Do(func(entity goalstate.Entity, deadline time.Time) {
			suite.Equal(suite.jobID.GetValue(), entity.GetID())
			updateEnt := entity.(*updateEntity)
			suite.Equal(suite.updateID.GetValue(), updateEnt.id.GetValue())
		})

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

func (suite *UpdateRunTestSuite) TestUpdateFailGetUpdate() {
	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(nil)

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

func (suite *UpdateRunTestSuite) TestUpdateFailGetJob() {
	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(nil)

	suite.cachedUpdate.EXPECT().
		Cancel(gomock.Any()).
		Return(fmt.Errorf("fake db error"))

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.EqualError(err, "fake db error")
}

func (suite *UpdateRunTestSuite) TestUpdateTaskAddTaskFail() {
	instancesTotal := []uint32{2, 3, 4, 5}

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		GetWorkflowType().
		Return(models.WorkflowType_UPDATE).
		AnyTimes()

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  instancesTotal,
			JobVersion: uint64(1),
		})

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(instancesTotal)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(&pbupdate.UpdateConfig{})

	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), gomock.Any()).
		Return(nil, fmt.Errorf("fake db error"))

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.EqualError(err, "fake db error")
}

func (suite *UpdateRunTestSuite) TestUpdateTaskRuntimeGetFail() {
	instancesTotal := []uint32{2, 3, 4, 5}

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		GetWorkflowType().
		Return(models.WorkflowType_UPDATE).
		AnyTimes()

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  instancesTotal,
			JobVersion: uint64(1),
		})

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(instancesTotal)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return([]uint32{})

	suite.cachedJob.EXPECT().
		AddTask(gomock.Any(), gomock.Any()).
		Return(suite.cachedTask, nil)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(&pbupdate.UpdateConfig{})

	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).
		Return(nil, fmt.Errorf("fake db error"))

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.EqualError(err, "fake db error")
}

func (suite *UpdateRunTestSuite) TestUpdateProgressDBError() {
	newJobVer := uint64(3)
	var instancesRemaining []uint32
	instancesTotal := []uint32{2, 3, 4, 5}
	runtime := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_RUNNING,
		GoalState:            pbtask.TaskState_RUNNING,
		Healthy:              pbtask.HealthState_HEALTHY,
		ConfigVersion:        newJobVer,
		DesiredConfigVersion: newJobVer,
	}

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob)

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  instancesTotal,
			JobVersion: newJobVer,
		})

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(instancesTotal)

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(instancesTotal)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(nil).Times(2)

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(&pbupdate.UpdateConfig{
			BatchSize: 0,
		}).
		Times(3)

	for _, instID := range instancesTotal {
		suite.cachedJob.EXPECT().
			AddTask(gomock.Any(), instID).
			Return(suite.cachedTask, nil)

		suite.cachedTask.EXPECT().
			GetRunTime(gomock.Any()).
			Return(runtime, nil)

		suite.cachedUpdate.EXPECT().
			IsInstanceComplete(newJobVer, runtime).
			Return(true)
	}

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		})

	suite.cachedUpdate.EXPECT().
		WriteProgress(
			gomock.Any(),
			pbupdate.State_ROLLING_FORWARD,
			[]uint32{2, 3, 4, 5},
			instancesRemaining,
			gomock.Any(),
		).Return(fmt.Errorf("fake db error"))

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.EqualError(err, "fake db error")
}

// TestUpdateRunFullyRunningAddInstances test add instances for a fully running
// job
func (suite *UpdateRunTestSuite) TestUpdateRunFullyRunningAddInstances() {
	oldInstanceNumber := uint32(10)
	newInstanceNumber := uint32(20)
	batchSize := uint32(5)
	newJobVersion := uint64(4)

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		GetWorkflowType().
		Return(models.WorkflowType_UPDATE).
		AnyTimes()

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  newSlice(oldInstanceNumber, newInstanceNumber),
			JobVersion: newJobVersion,
		}).AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(newSlice(oldInstanceNumber, newInstanceNumber))

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(nil).Times(2)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(&pbupdate.UpdateConfig{
			BatchSize: batchSize,
		}).AnyTimes()

	suite.jobStore.EXPECT().
		GetJobConfigWithVersion(gomock.Any(), gomock.Any(), uint64(4)).
		Return(&pbjob.JobConfig{
			ChangeLog: &peloton.ChangeLog{Version: uint64(4)},
		}, &models.ConfigAddOn{},
			nil)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&pbjob.RuntimeInfo{State: pbjob.JobState_RUNNING}, nil)

	for _, instID := range newSlice(oldInstanceNumber, oldInstanceNumber+batchSize) {
		suite.cachedJob.EXPECT().
			GetTask(instID).
			Return(suite.cachedTask)
		suite.cachedTask.EXPECT().
			GetRunTime(gomock.Any()).
			Return(nil, yarpcerrors.NotFoundErrorf("not found"))
		suite.taskStore.EXPECT().
			GetPodEvents(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil, nil)
	}

	suite.cachedJob.EXPECT().
		CreateTasks(gomock.Any(), gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, runtimes map[uint32]*pbtask.RuntimeInfo, _ string) {
			suite.Len(runtimes, int(batchSize))
		}).Return(nil)

	suite.resmgrClient.EXPECT().
		EnqueueGangs(gomock.Any(), gomock.Any()).
		Return(&resmgrsvc.EnqueueGangsResponse{}, nil)

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		})

	suite.cachedUpdate.EXPECT().
		WriteProgress(
			gomock.Any(),
			pbupdate.State_ROLLING_FORWARD,
			gomock.Any(),
			gomock.Any(),
			gomock.Any()).
		Do(func(_ context.Context, _ pbupdate.State,
			instancesDone []uint32, instancesFailed []uint32, instancesCurrent []uint32) {
			suite.EqualValues(instancesCurrent,
				newSlice(oldInstanceNumber, oldInstanceNumber+batchSize))
			suite.Empty(instancesFailed)
			suite.Empty(instancesDone)
		}).Return(nil)

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

// TestUpdateRun_FullyRunning_AddShrinkInstances test adding shrink instances
// for a fully running job
func (suite *UpdateRunTestSuite) TestUpdateRun_FullyRunning_AddShrinkInstances() {
	oldInstanceNumber := uint32(10)
	newInstanceNumber := uint32(20)
	batchSize := uint32(5)
	newJobVersion := uint64(4)

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		GetWorkflowType().
		Return(models.WorkflowType_UPDATE).
		AnyTimes()

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  newSlice(oldInstanceNumber, newInstanceNumber),
			JobVersion: newJobVersion,
		}).AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(newSlice(oldInstanceNumber, newInstanceNumber))

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(nil).Times(2)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(&pbupdate.UpdateConfig{
			BatchSize: batchSize,
		}).AnyTimes()

	suite.jobStore.EXPECT().
		GetJobConfigWithVersion(gomock.Any(), gomock.Any(), uint64(4)).
		Return(&pbjob.JobConfig{
			ChangeLog: &peloton.ChangeLog{Version: uint64(4)},
		}, &models.ConfigAddOn{},
			nil)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&pbjob.RuntimeInfo{State: pbjob.JobState_RUNNING}, nil)

	for _, instID := range newSlice(oldInstanceNumber, oldInstanceNumber+batchSize) {
		suite.cachedJob.EXPECT().
			GetTask(instID).
			Return(nil)

		mesosTaskID := fmt.Sprintf("%s-%d-%d", suite.jobID.GetValue(), instID, 50)
		taskID := &mesos.TaskID{
			Value: &mesosTaskID,
		}

		prevMesosTaskID := fmt.Sprintf("%s-%d-%d", suite.jobID.GetValue(), instID, 49)
		prevTaskID := &mesos.TaskID{
			Value: &prevMesosTaskID,
		}

		var podEvents []*pbtask.PodEvent
		podEvent := &pbtask.PodEvent{
			TaskId:     taskID,
			PrevTaskId: prevTaskID,
		}
		podEvents = append(podEvents, podEvent)

		suite.taskStore.EXPECT().
			GetPodEvents(gomock.Any(), suite.jobID, uint32(instID), uint64(1)).
			Return(podEvents, nil)
	}

	suite.cachedJob.EXPECT().
		CreateTasks(gomock.Any(), gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, runtimes map[uint32]*pbtask.RuntimeInfo, _ string) {
			suite.Len(runtimes, int(batchSize))
		}).Return(nil)

	suite.resmgrClient.EXPECT().
		EnqueueGangs(gomock.Any(), gomock.Any()).
		Return(&resmgrsvc.EnqueueGangsResponse{}, nil)

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		})

	suite.cachedUpdate.EXPECT().
		WriteProgress(
			gomock.Any(),
			pbupdate.State_ROLLING_FORWARD,
			gomock.Any(),
			gomock.Any(),
			gomock.Any()).
		Do(func(_ context.Context, _ pbupdate.State,
			instancesDone []uint32, instancesFailed []uint32, instancesCurrent []uint32) {
			suite.EqualValues(instancesCurrent,
				newSlice(oldInstanceNumber, oldInstanceNumber+batchSize))
			suite.Empty(instancesDone)
		}).Return(nil)

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

// TestUpdateRunFullyRunningUpdateInstances test update instances for a fully running
// job
func (suite *UpdateRunTestSuite) TestUpdateRunFullyRunningUpdateInstances() {
	instanceNumber := uint32(10)
	batchSize := uint32(5)
	jobVersion := uint64(3)
	newJobVersion := uint64(4)
	updateConfig := &pbupdate.UpdateConfig{
		BatchSize: batchSize,
	}

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID).
		AnyTimes()

	for _, instID := range newSlice(0, batchSize) {
		suite.cachedJob.EXPECT().
			AddTask(gomock.Any(), instID).
			Return(suite.cachedTask, nil)
		suite.cachedTask.EXPECT().
			GetRunTime(gomock.Any()).
			Return(&pbtask.RuntimeInfo{
				State:                pbtask.TaskState_RUNNING,
				ConfigVersion:        jobVersion,
				DesiredConfigVersion: jobVersion,
			}, nil)
	}

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  newSlice(0, instanceNumber),
			JobVersion: newJobVersion,
		}).AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(newSlice(0, batchSize))

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(newSlice(0, instanceNumber))

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(nil).Times(2)

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(updateConfig).AnyTimes()

	suite.jobStore.EXPECT().
		GetJobConfigWithVersion(gomock.Any(), gomock.Any(), newJobVersion).
		Return(&pbjob.JobConfig{
			ChangeLog: &peloton.ChangeLog{Version: newJobVersion},
		}, &models.ConfigAddOn{},
			nil)

	for range newSlice(0, batchSize) {
		runtime := &pbtask.RuntimeInfo{
			State:                pbtask.TaskState_RUNNING,
			ConfigVersion:        jobVersion,
			DesiredConfigVersion: jobVersion,
		}

		suite.cachedUpdate.EXPECT().
			IsInstanceComplete(newJobVersion, runtime).
			Return(false)

		suite.cachedUpdate.EXPECT().
			IsInstanceInProgress(newJobVersion, runtime).
			Return(false)

		suite.cachedUpdate.EXPECT().
			IsInstanceFailed(runtime, updateConfig.GetMaxInstanceAttempts()).
			Return(false)

		suite.cachedUpdate.EXPECT().
			GetRuntimeDiff(gomock.Any()).
			Return(jobmgrcommon.RuntimeDiff{
				jobmgrcommon.DesiredConfigVersionField: newJobVersion,
			})
	}

	suite.taskGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return().
		Times(int(batchSize))

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		})

	suite.cachedUpdate.EXPECT().
		WriteProgress(
			gomock.Any(),
			pbupdate.State_ROLLING_FORWARD,
			gomock.Any(),
			gomock.Any(),
			gomock.Any()).
		Do(func(_ context.Context, _ pbupdate.State,
			instancesDone []uint32, instancesFailed []uint32, instancesCurrent []uint32) {
			suite.EqualValues(instancesCurrent,
				newSlice(0, batchSize))
			suite.Empty(instancesFailed)
			suite.Empty(instancesDone)
		}).Return(nil)

	for _, instID := range newSlice(0, batchSize) {
		suite.cachedJob.EXPECT().
			GetTask(instID).
			Return(suite.cachedTask)
		suite.cachedTask.EXPECT().
			GetRunTime(gomock.Any()).
			Return(&pbtask.RuntimeInfo{
				State:                pbtask.TaskState_RUNNING,
				ConfigVersion:        jobVersion,
				DesiredConfigVersion: jobVersion,
			}, nil)
	}

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

// TestUpdateRunContainsKilledTaskUpdateInstances tests the case to update
// a job with killed tasks
func (suite *UpdateRunTestSuite) TestUpdateRunContainsKilledTaskUpdateInstances() {
	instanceNumber := uint32(10)
	batchSize := uint32(5)
	jobVersion := uint64(3)
	newJobVersion := uint64(4)

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  newSlice(0, instanceNumber),
			JobVersion: newJobVersion,
		}).AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(newSlice(0, instanceNumber))

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(nil).Times(2)

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(&pbupdate.UpdateConfig{
			BatchSize: batchSize,
		}).AnyTimes()

	suite.jobStore.EXPECT().
		GetJobConfigWithVersion(gomock.Any(), gomock.Any(), newJobVersion).
		Return(&pbjob.JobConfig{
			ChangeLog: &peloton.ChangeLog{Version: newJobVersion},
		}, &models.ConfigAddOn{},
			nil)

	suite.taskGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return().
		Times(int(batchSize))

	suite.cachedUpdate.EXPECT().
		GetRuntimeDiff(gomock.Any()).
		Return(jobmgrcommon.RuntimeDiff{
			jobmgrcommon.DesiredConfigVersionField: newJobVersion,
		}).Times(int(batchSize))

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		})

	suite.cachedUpdate.EXPECT().
		WriteProgress(
			gomock.Any(),
			pbupdate.State_ROLLING_FORWARD,
			gomock.Any(),
			gomock.Any(),
			gomock.Any()).
		Do(func(_ context.Context, _ pbupdate.State,
			instancesDone []uint32, instancesFailed []uint32, instancesCurrent []uint32) {
			suite.EqualValues(instancesCurrent,
				newSlice(0, batchSize))
			suite.Empty(instancesDone)
			suite.Empty(instancesFailed)
		}).Return(nil)

	suite.cachedUpdate.EXPECT().
		ID().
		Return(suite.updateID)

	suite.cachedJob.EXPECT().
		GetTask(uint32(0)).
		Return(suite.cachedTask)

	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).
		Return(&pbtask.RuntimeInfo{
			State:                pbtask.TaskState_KILLED,
			GoalState:            pbtask.TaskState_KILLED,
			ConfigVersion:        jobVersion,
			DesiredConfigVersion: jobVersion,
		}, nil)

	suite.updateGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

// TestUpdateRunContainsTerminatedTaskInstances tests the case to update
// a job with failed tasks with killed goal state
func (suite *UpdateRunTestSuite) TestUpdateRunContainsTerminatedTaskInstances() {
	instanceNumber := uint32(10)
	batchSize := uint32(5)
	jobVersion := uint64(3)
	newJobVersion := uint64(4)

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  newSlice(0, instanceNumber),
			JobVersion: newJobVersion,
		}).AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(newSlice(0, instanceNumber))

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(nil).Times(2)

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(&pbupdate.UpdateConfig{
			BatchSize: batchSize,
		}).AnyTimes()

	suite.jobStore.EXPECT().
		GetJobConfigWithVersion(gomock.Any(), gomock.Any(), newJobVersion).
		Return(&pbjob.JobConfig{
			ChangeLog: &peloton.ChangeLog{Version: newJobVersion},
		}, &models.ConfigAddOn{},
			nil)

	suite.taskGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return().
		Times(int(batchSize))

	suite.cachedUpdate.EXPECT().
		GetRuntimeDiff(gomock.Any()).
		Return(jobmgrcommon.RuntimeDiff{
			jobmgrcommon.DesiredConfigVersionField: newJobVersion,
		}).Times(int(batchSize))

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		})

	suite.cachedUpdate.EXPECT().
		WriteProgress(
			gomock.Any(),
			pbupdate.State_ROLLING_FORWARD,
			gomock.Any(),
			gomock.Any(),
			gomock.Any()).
		Do(func(_ context.Context, _ pbupdate.State,
			instancesDone []uint32, instancesFailed []uint32, instancesCurrent []uint32) {
			suite.EqualValues(instancesCurrent,
				newSlice(0, batchSize))
			suite.Empty(instancesDone)
			suite.Empty(instancesFailed)
		}).Return(nil)

	suite.cachedUpdate.EXPECT().
		ID().
		Return(suite.updateID)

	suite.cachedJob.EXPECT().
		GetTask(uint32(0)).
		Return(suite.cachedTask)

	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).
		Return(&pbtask.RuntimeInfo{
			State:                pbtask.TaskState_FAILED,
			GoalState:            pbtask.TaskState_KILLED,
			ConfigVersion:        jobVersion,
			DesiredConfigVersion: jobVersion,
		}, nil)

	suite.updateGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

// TestUpdateRunKilledJobAddInstances tests add instances to killed job
func (suite *UpdateRunTestSuite) TestUpdateRunKilledJobAddInstances() {
	oldInstanceNumber := uint32(10)
	newInstanceNumber := uint32(20)
	batchSize := uint32(5)

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		GetWorkflowType().
		Return(models.WorkflowType_UPDATE).
		AnyTimes()

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  newSlice(oldInstanceNumber, newInstanceNumber),
			JobVersion: uint64(4),
		}).AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(newSlice(oldInstanceNumber, newInstanceNumber))

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(newSlice(oldInstanceNumber, newInstanceNumber))

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(nil).Times(2)

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(&pbupdate.UpdateConfig{
			BatchSize: batchSize,
		}).AnyTimes()

	suite.jobStore.EXPECT().
		GetJobConfigWithVersion(gomock.Any(), gomock.Any(), uint64(4)).
		Return(&pbjob.JobConfig{
			ChangeLog: &peloton.ChangeLog{Version: uint64(4)},
		}, &models.ConfigAddOn{},
			nil)

	for _, instID := range newSlice(oldInstanceNumber, newInstanceNumber) {
		suite.cachedJob.EXPECT().
			AddTask(gomock.Any(), instID).
			Return(nil,
				yarpcerrors.NotFoundErrorf("new instance has no runtime yet"))
	}

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&pbjob.RuntimeInfo{GoalState: pbjob.JobState_KILLED}, nil)

	suite.cachedJob.EXPECT().
		Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).
		Do(func(_ context.Context, jobInfo *pbjob.JobInfo, _ *models.ConfigAddOn, _ cached.UpdateRequest) {
			suite.Equal(jobInfo.Runtime.GoalState,
				goalstateutil.GetDefaultJobGoalState(pbjob.JobType_SERVICE))
		}).
		Return(nil)

	for _, instID := range newSlice(oldInstanceNumber, oldInstanceNumber+batchSize) {
		suite.cachedJob.EXPECT().
			GetTask(instID).
			Return(nil)
		suite.taskStore.EXPECT().
			GetPodEvents(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil, nil)
	}

	suite.cachedJob.EXPECT().
		CreateTasks(gomock.Any(), gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, runtimes map[uint32]*pbtask.RuntimeInfo, _ string) {
			suite.Len(runtimes, int(batchSize))
		}).Return(nil)

	suite.resmgrClient.EXPECT().
		EnqueueGangs(gomock.Any(), gomock.Any()).
		Return(&resmgrsvc.EnqueueGangsResponse{}, nil)

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		})

	suite.cachedUpdate.EXPECT().
		WriteProgress(
			gomock.Any(),
			pbupdate.State_ROLLING_FORWARD,
			gomock.Any(),
			gomock.Any(),
			gomock.Any()).
		Do(func(_ context.Context, _ pbupdate.State,
			instancesDone []uint32, instancesFailed []uint32, instancesCurrent []uint32) {
			suite.EqualValues(instancesCurrent,
				newSlice(oldInstanceNumber, oldInstanceNumber+batchSize))
			suite.Empty(instancesDone)
			suite.Empty(instancesFailed)
		}).Return(nil)

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

// TestUpdateRunDBErrorAddInstances test add instances and db has error
// when add the tasks
func (suite *UpdateRunTestSuite) TestUpdateRunDBErrorAddInstances() {
	oldInstanceNumber := uint32(10)
	newInstanceNumber := uint32(20)
	batchSize := uint32(5)

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  newSlice(oldInstanceNumber, newInstanceNumber),
			JobVersion: uint64(4),
		}).AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(newSlice(oldInstanceNumber, newInstanceNumber))

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(nil).Times(2)

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetWorkflowType().
		Return(models.WorkflowType_UPDATE).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(&pbupdate.UpdateConfig{
			BatchSize: batchSize,
		}).AnyTimes()

	suite.jobStore.EXPECT().
		GetJobConfigWithVersion(gomock.Any(), gomock.Any(), uint64(4)).
		Return(&pbjob.JobConfig{
			ChangeLog: &peloton.ChangeLog{Version: uint64(4)},
		}, &models.ConfigAddOn{},
			nil)

	suite.cachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(&pbjob.RuntimeInfo{State: pbjob.JobState_RUNNING}, nil)

	for _, instID := range newSlice(oldInstanceNumber, oldInstanceNumber+batchSize) {
		suite.cachedJob.EXPECT().
			GetTask(instID).
			Return(nil)
		suite.taskStore.EXPECT().
			GetPodEvents(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil, nil)
	}

	suite.cachedJob.EXPECT().
		CreateTasks(gomock.Any(), gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, runtimes map[uint32]*pbtask.RuntimeInfo, _ string) {
			suite.Len(runtimes, int(batchSize))
		}).Return(nil)

	suite.resmgrClient.EXPECT().
		EnqueueGangs(gomock.Any(), gomock.Any()).
		Return(&resmgrsvc.EnqueueGangsResponse{}, nil)

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Return(yarpcerrors.UnavailableErrorf("test error"))

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.Error(err)
}

// TestUpdateRunDBErrorUpdateInstances test add instances and db has error
// when update the tasks
func (suite *UpdateRunTestSuite) TestUpdateRunDBErrorUpdateInstances() {
	instanceNumber := uint32(10)
	batchSize := uint32(5)
	newJobVersion := uint64(4)

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  newSlice(0, instanceNumber),
			JobVersion: newJobVersion,
		}).AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(newSlice(0, instanceNumber))

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(nil).Times(2)

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(&pbupdate.UpdateConfig{
			BatchSize: batchSize,
		}).AnyTimes()

	suite.jobStore.EXPECT().
		GetJobConfigWithVersion(gomock.Any(), gomock.Any(), newJobVersion).
		Return(&pbjob.JobConfig{
			ChangeLog: &peloton.ChangeLog{Version: newJobVersion},
		}, &models.ConfigAddOn{},
			nil)

	suite.cachedUpdate.EXPECT().
		GetRuntimeDiff(gomock.Any()).
		Return(jobmgrcommon.RuntimeDiff{
			jobmgrcommon.DesiredConfigVersionField: newJobVersion,
		}).Times(int(batchSize))

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Return(yarpcerrors.UnavailableErrorf("test error"))

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.Error(err)
}

// TestRunningUpdateRemoveInstances tests removing instances
func (suite *UpdateRunTestSuite) TestRunningUpdateRemoveInstances() {
	instancesTotal := []uint32{4, 5, 6}
	oldJobConfigVer := uint64(3)
	newJobConfigVer := uint64(4)

	runtimeTerminated := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_KILLED,
		GoalState:            pbtask.TaskState_KILLED,
		Healthy:              pbtask.HealthState_INVALID,
		ConfigVersion:        oldJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  instancesTotal,
			JobVersion: newJobConfigVer,
		}).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(instancesTotal).Times(2)

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(&pbupdate.UpdateConfig{
			BatchSize: 2,
		}).AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.jobStore.EXPECT().
		GetJobConfigWithVersion(gomock.Any(), gomock.Any(), uint64(4)).
		Return(&pbjob.JobConfig{
			ChangeLog: &peloton.ChangeLog{Version: uint64(4)},
		}, &models.ConfigAddOn{},
			nil)

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, runtimes map[uint32]jobmgrcommon.RuntimeDiff) {
			suite.Equal(2, len(runtimes))
			for _, runtime := range runtimes {
				suite.Equal(runtime[jobmgrcommon.GoalStateField],
					pbtask.TaskState_DELETED)
				suite.Equal(runtime[jobmgrcommon.DesiredConfigVersionField],
					newJobConfigVer)
			}
		}).
		Return(nil)

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID).AnyTimes()

	suite.taskGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return().
		Times(2)

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		})

	suite.cachedUpdate.EXPECT().
		WriteProgress(
			gomock.Any(),
			pbupdate.State_ROLLING_FORWARD,
			gomock.Any(),
			gomock.Any(),
			gomock.Any(),
		).Return(nil)

	suite.cachedJob.EXPECT().
		GetTask(gomock.Any()).
		Return(suite.cachedTask)

	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).
		Return(runtimeTerminated, nil)

	suite.cachedUpdate.EXPECT().
		ID().
		Return(suite.updateID)

	suite.updateGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

// TestRunningUpdateRemoveInstancesDBError tests removing instances with DB error
// during patch tasks
func (suite *UpdateRunTestSuite) TestRunningUpdateRemoveInstancesDBError() {
	instancesTotal := []uint32{4, 5, 6}
	newJobConfigVer := uint64(4)

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  instancesTotal,
			JobVersion: newJobConfigVer,
		}).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(instancesTotal).Times(2)

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(&pbupdate.UpdateConfig{
			BatchSize: 2,
		}).AnyTimes()

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID).AnyTimes()

	suite.jobStore.EXPECT().
		GetJobConfigWithVersion(gomock.Any(), gomock.Any(), uint64(4)).
		Return(&pbjob.JobConfig{
			ChangeLog: &peloton.ChangeLog{Version: uint64(4)},
		}, &models.ConfigAddOn{},
			nil)

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Return(fmt.Errorf("fake db error"))

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.EqualError(err, "fake db error")
}

// TestRunningUpdateFailed tests the case that update fails due to
// too many instances failed
func (suite *UpdateRunTestSuite) TestRunningUpdateFailed() {
	instancesTotal := []uint32{0, 1, 2, 3, 4, 5, 6}
	newJobConfigVer := uint64(4)
	failureCount := uint32(5)
	failedInstances := uint32(3)

	updateConfig := &pbupdate.UpdateConfig{
		BatchSize:           0,
		MaxFailureInstances: failedInstances,
	}

	runtimeFailed := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_FAILED,
		GoalState:            pbtask.TaskState_RUNNING,
		FailureCount:         failureCount,
		Healthy:              pbtask.HealthState_UNHEALTHY,
		ConfigVersion:        newJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	runtimeDone := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_RUNNING,
		GoalState:            pbtask.TaskState_RUNNING,
		Healthy:              pbtask.HealthState_HEALTHY,
		ConfigVersion:        newJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  instancesTotal,
			JobVersion: uint64(4),
		}).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(instancesTotal)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(updateConfig).
		Times(4)

	for i, instID := range instancesTotal {
		suite.cachedJob.EXPECT().
			AddTask(gomock.Any(), instID).
			Return(suite.cachedTask, nil)

		if uint32(i) < failedInstances {
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeFailed, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeFailed).
				Return(false)
			suite.cachedUpdate.EXPECT().
				IsInstanceFailed(runtimeFailed, updateConfig.GetMaxInstanceAttempts()).
				Return(true)
		} else {
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeDone, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeDone).
				Return(true)
		}
	}

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		WriteProgress(
			gomock.Any(),
			pbupdate.State_FAILED,
			newSlice(failedInstances, uint32(len(instancesTotal))),
			newSlice(0, failedInstances),
			nil,
		).Return(nil)

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		ID().
		Return(suite.updateID)

	suite.updateGoalStateEngine.
		EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

// TestRunningUpdateRolledBack tests the case that update fails due to
// too many instances failed and rollback is triggered
func (suite *UpdateRunTestSuite) TestRunningUpdateRolledBack() {
	totalInstances := uint32(10)
	totalInstancesToUpdate := []uint32{0, 1, 2, 3, 4, 5, 6}
	newJobConfigVer := uint64(4)
	failureCount := uint32(5)
	failedInstances := uint32(3)

	updateConfig := &pbupdate.UpdateConfig{
		BatchSize:           0,
		MaxFailureInstances: failedInstances,
		RollbackOnFailure:   true,
	}

	runtimeFailed := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_FAILED,
		GoalState:            pbtask.TaskState_RUNNING,
		FailureCount:         failureCount,
		Healthy:              pbtask.HealthState_UNHEALTHY,
		ConfigVersion:        newJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	runtimeDone := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_RUNNING,
		GoalState:            pbtask.TaskState_RUNNING,
		Healthy:              pbtask.HealthState_HEALTHY,
		ConfigVersion:        newJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  totalInstancesToUpdate,
			JobVersion: uint64(4),
		}).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(totalInstancesToUpdate)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(updateConfig).
		Times(4)

	for i, instID := range totalInstancesToUpdate {
		suite.cachedJob.EXPECT().
			AddTask(gomock.Any(), instID).
			Return(suite.cachedTask, nil)

		if uint32(i) < failedInstances {
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeFailed, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeFailed).
				Return(false)
			suite.cachedUpdate.EXPECT().
				IsInstanceFailed(runtimeFailed, updateConfig.GetMaxInstanceAttempts()).
				Return(true)
		} else {
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeDone, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeDone).
				Return(true)
		}
	}

	suite.cachedUpdate.EXPECT().
		GetWorkflowType().
		Return(models.WorkflowType_UPDATE)

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		})

	suite.cachedUpdate.EXPECT().
		Rollback(gomock.Any()).
		Return(nil)

	suite.cachedJob.EXPECT().
		GetConfig(gomock.Any()).
		Return(&pbjob.JobConfig{
			InstanceCount: totalInstances,
		}, nil)

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, runtimeDiffs map[uint32]jobmgrcommon.RuntimeDiff) {
			suite.Len(runtimeDiffs, int(totalInstances)-len(totalInstancesToUpdate))
			for i := uint32(len(totalInstancesToUpdate)); i < totalInstances; i++ {
				suite.NotEmpty(runtimeDiffs[i])
			}
		}).
		Return(nil)

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID).
		Times(2)

	suite.cachedUpdate.EXPECT().
		ID().
		Return(suite.updateID).
		Times(2)

	suite.updateGoalStateEngine.
		EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

// TestRunningUpdateRolledBack tests the case that update fails due to
// too many instances failed and rollback is triggered but fails
func (suite *UpdateRunTestSuite) TestRunningUpdateRolledBackFail() {
	instancesTotal := []uint32{0, 1, 2, 3, 4, 5, 6}
	newJobConfigVer := uint64(4)
	failureCount := uint32(5)
	failedInstances := uint32(3)

	updateConfig := &pbupdate.UpdateConfig{
		BatchSize:           0,
		MaxFailureInstances: failedInstances,
		RollbackOnFailure:   true,
	}

	runtimeFailed := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_FAILED,
		GoalState:            pbtask.TaskState_RUNNING,
		FailureCount:         failureCount,
		Healthy:              pbtask.HealthState_UNHEALTHY,
		ConfigVersion:        newJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	runtimeDone := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_RUNNING,
		GoalState:            pbtask.TaskState_RUNNING,
		Healthy:              pbtask.HealthState_HEALTHY,
		ConfigVersion:        newJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  instancesTotal,
			JobVersion: uint64(4),
		}).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(instancesTotal)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(updateConfig).
		Times(4)

	for i, instID := range instancesTotal {
		suite.cachedJob.EXPECT().
			AddTask(gomock.Any(), instID).
			Return(suite.cachedTask, nil)

		if uint32(i) < failedInstances {
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeFailed, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeFailed).
				Return(false)
			suite.cachedUpdate.EXPECT().
				IsInstanceFailed(runtimeFailed, updateConfig.GetMaxInstanceAttempts()).
				Return(true)
		} else {
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeDone, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeDone).
				Return(true)
		}
	}

	suite.cachedUpdate.EXPECT().
		GetWorkflowType().
		Return(models.WorkflowType_UPDATE)

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		})

	suite.cachedUpdate.EXPECT().
		Rollback(gomock.Any()).
		Return(fmt.Errorf("test error"))

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		ID().
		Return(suite.updateID)

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.Error(err)
}

// TestUpdateRollingBackFailed tests the case that update rollback
// failed due to too many failure
func (suite *UpdateRunTestSuite) TestUpdateRollingBackFailed() {
	instancesTotal := []uint32{0, 1, 2, 3, 4, 5, 6}
	newJobConfigVer := uint64(4)
	failureCount := uint32(5)
	failedInstances := uint32(3)

	updateConfig := &pbupdate.UpdateConfig{
		BatchSize:           0,
		MaxFailureInstances: failedInstances,
		RollbackOnFailure:   true,
	}

	runtimeFailed := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_FAILED,
		GoalState:            pbtask.TaskState_RUNNING,
		FailureCount:         failureCount,
		Healthy:              pbtask.HealthState_UNHEALTHY,
		ConfigVersion:        newJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	runtimeDone := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_RUNNING,
		GoalState:            pbtask.TaskState_RUNNING,
		Healthy:              pbtask.HealthState_HEALTHY,
		ConfigVersion:        newJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  instancesTotal,
			JobVersion: uint64(4),
		}).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(instancesTotal)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(updateConfig).
		Times(4)

	for i, instID := range instancesTotal {
		suite.cachedJob.EXPECT().
			AddTask(gomock.Any(), instID).
			Return(suite.cachedTask, nil)

		if uint32(i) < failedInstances {
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeFailed, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeFailed).
				Return(false)
			suite.cachedUpdate.EXPECT().
				IsInstanceFailed(runtimeFailed, updateConfig.GetMaxInstanceAttempts()).
				Return(true)
		} else {
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeDone, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeDone).
				Return(true)
		}
	}

	suite.cachedUpdate.EXPECT().
		GetWorkflowType().
		Return(models.WorkflowType_UPDATE)

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_BACKWARD,
		})

	suite.cachedUpdate.EXPECT().
		WriteProgress(
			gomock.Any(),
			pbupdate.State_FAILED,
			newSlice(failedInstances, uint32(len(instancesTotal))),
			newSlice(0, failedInstances),
			nil,
		).Return(nil)

	suite.cachedJob.EXPECT().
		ID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		ID().
		Return(suite.updateID)

	suite.updateGoalStateEngine.
		EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

// TestRunningUpdatePartialFailure tests the case that some instances
// in the job failed but has not reached updateConfig.MaxFailureInstances
func (suite *UpdateRunTestSuite) TestRunningUpdatePartialFailure() {
	instancesTotal := []uint32{0, 1, 2, 3, 4, 5, 6}
	newJobConfigVer := uint64(4)
	failureCount := uint32(5)
	failedInstances := uint32(3)

	updateConfig := &pbupdate.UpdateConfig{
		BatchSize:           0,
		MaxFailureInstances: failedInstances + 1,
	}

	runtimeFailed := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_FAILED,
		GoalState:            pbtask.TaskState_RUNNING,
		FailureCount:         failureCount,
		Healthy:              pbtask.HealthState_UNHEALTHY,
		ConfigVersion:        newJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	runtimeDone := &pbtask.RuntimeInfo{
		State:                pbtask.TaskState_RUNNING,
		GoalState:            pbtask.TaskState_RUNNING,
		Healthy:              pbtask.HealthState_HEALTHY,
		ConfigVersion:        newJobConfigVer,
		DesiredConfigVersion: newJobConfigVer,
	}

	suite.updateFactory.EXPECT().
		GetUpdate(suite.updateID).
		Return(suite.cachedUpdate)

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).
		Return(suite.cachedJob).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		JobID().
		Return(suite.jobID)

	suite.cachedUpdate.EXPECT().
		GetGoalState().
		Return(&cached.UpdateStateVector{
			Instances:  instancesTotal,
			JobVersion: uint64(4),
		}).
		AnyTimes()

	suite.cachedUpdate.EXPECT().
		GetInstancesFailed().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesDone().
		Return([]uint32{})

	suite.cachedUpdate.EXPECT().
		GetInstancesCurrent().
		Return(instancesTotal)

	suite.cachedUpdate.EXPECT().
		GetInstancesAdded().
		Return(nil)

	suite.cachedUpdate.EXPECT().
		GetInstancesUpdated().
		Return(instancesTotal)

	suite.cachedUpdate.EXPECT().
		GetInstancesRemoved().
		Return(nil).Times(2)

	suite.cachedUpdate.EXPECT().
		GetUpdateConfig().
		Return(updateConfig).
		Times(4)

	for i, instID := range instancesTotal {
		suite.cachedJob.EXPECT().
			AddTask(gomock.Any(), instID).
			Return(suite.cachedTask, nil)

		if uint32(i) < failedInstances {
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeFailed, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeFailed).
				Return(false)
			suite.cachedUpdate.EXPECT().
				IsInstanceFailed(runtimeFailed, updateConfig.GetMaxInstanceAttempts()).
				Return(true)
		} else {
			suite.cachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(runtimeDone, nil)
			suite.cachedUpdate.EXPECT().
				IsInstanceComplete(newJobConfigVer, runtimeDone).
				Return(true)
		}
	}

	suite.cachedUpdate.EXPECT().
		GetState().
		Return(&cached.UpdateStateVector{
			State: pbupdate.State_ROLLING_FORWARD,
		})

	suite.cachedUpdate.EXPECT().
		WriteProgress(
			gomock.Any(),
			pbupdate.State_ROLLING_FORWARD,
			newSlice(failedInstances, uint32(len(instancesTotal))),
			newSlice(0, failedInstances),
			nil,
		).Return(nil)

	err := UpdateRun(context.Background(), suite.updateEnt)
	suite.NoError(err)
}

func newSlice(start uint32, end uint32) []uint32 {
	result := make([]uint32, 0, end-start)
	for i := start; i < end; i++ {
		result = append(result, i)
	}
	return result
}
