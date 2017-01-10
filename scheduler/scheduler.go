// Scheduler Interface
// IN: job
// OUT: placement decision <task, node>
// https://github.com/Netflix/Fenzo

package scheduler

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	master_task "code.uber.internal/infra/peloton/master/task"
	sched_config "code.uber.internal/infra/peloton/scheduler/config"
	sched_metrics "code.uber.internal/infra/peloton/scheduler/metrics"
	"code.uber.internal/infra/peloton/util"
	"code.uber.internal/infra/peloton/yarpc/encoding/mpb"
	log "github.com/Sirupsen/logrus"
	"github.com/uber-go/tally"
	"go.uber.org/yarpc"
	"go.uber.org/yarpc/encoding/json"
	mesos "mesos/v1"
	"peloton/master/offerpool"
	"peloton/master/taskqueue"
	"peloton/task"
)

const (
	// GetOfferTimeout is the timeout value for get offer request
	GetOfferTimeout = 1 * time.Second
	// GetTaskTimeout is the timeout value for get task request
	GetTaskTimeout = 1 * time.Second
)

// InitManager inits the schedulerManager
func InitManager(d yarpc.Dispatcher, cfg *sched_config.Config, mesosClient mpb.Client, metrics *sched_metrics.Metrics) {
	s := schedulerManager{
		cfg:      cfg,
		launcher: master_task.GetTaskLauncher(d, mesosClient, metrics),
		client:   json.New(d.ClientConfig("peloton-master")),
		rootCtx:  context.Background(),
		metrics:  metrics,
	}
	s.Start()
}

type schedulerManager struct {
	dispatcher yarpc.Dispatcher
	cfg        *sched_config.Config
	client     json.Client
	rootCtx    context.Context
	started    int32
	shutdown   int32
	launcher   master_task.Launcher
	metrics    *sched_metrics.Metrics
}

func (s *schedulerManager) Start() {
	if atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		log.Infof("Scheduler started")
		s.metrics.Running.Update(1)
		go s.workLoop()
		return
	}
	log.Warnf("Scheduler already started")
}

func (s *schedulerManager) Stop() {
	log.Infof("Scheduler stopping")
	s.metrics.Running.Update(0)
	atomic.StoreInt32(&s.shutdown, 1)
}

func (s *schedulerManager) launchTasksLoop(tasks []*task.TaskInfo) {
	nTasks := len(tasks)
	for shutdown := atomic.LoadInt32(&s.shutdown); shutdown == 0; {
		offers, err := s.getOffers(s.cfg.OfferDequeueLimit)
		if err != nil {
			log.Errorf("Failed to dequeue offer, err=%v", err)
			s.metrics.OfferGetFail.Inc(1)
			time.Sleep(GetOfferTimeout)
			continue
		}
		if len(offers) == 0 {
			s.metrics.OfferStarved.Inc(1)
			time.Sleep(GetOfferTimeout)
			continue
		}
		s.metrics.OfferGet.Inc(1)
		// TODO: handle multiple offer -> multiple tasks assignment
		// for now only get one offer each time
		offer := offers[0]
		tasks = s.assignTasksToOffer(tasks, offer)
		if len(tasks) == 0 {
			break
		}
	}
	log.Debugf("Launched all %v tasks", nTasks)
}

func (s *schedulerManager) assignTasksToOffer(
	tasks []*task.TaskInfo, offer *mesos.Offer) []*task.TaskInfo {
	remain := util.GetOfferScalarResourceSummary(offer)
	offerID := offer.GetId().Value
	nTasks := len(tasks)
	var selectedTasks []*task.TaskInfo
	for i := 0; i < nTasks; i++ {
		ok := util.CanTakeTask(&remain, tasks[len(tasks)-1])
		if ok {
			selectedTasks = append(selectedTasks, tasks[len(tasks)-1])
			tasks = tasks[:len(tasks)-1]
		} else {
			break
		}
	}
	// launch the tasks that can be launched
	if len(selectedTasks) > 0 {
		err := s.launcher.LaunchTasks(offer, selectedTasks)
		if err != nil {
			// TODO: handle task launch error and reschedule the tasks
			log.Errorf("Failed to launch %d tasks: %v", len(selectedTasks), err)
			s.metrics.TaskLaunchDispatchesFail.Inc(1)
			return tasks
		}
		s.metrics.TaskLaunchDispatches.Inc(1)

		log.Infof("Launched %v tasks on %v using offer %v", len(selectedTasks),
			offer.GetHostname(), *offerID)
	}
	return tasks
}

// workLoop is the internal loop that
func (s *schedulerManager) workLoop() {
	for shutdown := atomic.LoadInt32(&s.shutdown); shutdown == 0; {
		tasks, err := s.getTasks(s.cfg.TaskDequeueLimit)
		if err != nil {
			log.Errorf("Failed to dequeue tasks, err=%v", err)
			time.Sleep(GetTaskTimeout)
			continue
		}
		if len(tasks) == 0 {
			time.Sleep(GetTaskTimeout)
			continue
		}
		log.Infof("Dequeued %v tasks from task queue", len(tasks))
		s.launchTasksLoop(tasks)
	}
}

func (s *schedulerManager) getTasks(limit int) (
	taskInfos []*task.TaskInfo, err error) {
	// It could happen that the work loop is started before the
	// peloton master inbound is started.  In such case it could
	// panic. This we capture the panic, return error, wait then
	// resume
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Recovered from panic %v", r)
		}
	}()

	ctx, cancelFunc := context.WithTimeout(s.rootCtx, 10*time.Second)
	defer cancelFunc()
	var response taskqueue.DequeueResponse
	var request = &taskqueue.DequeueRequest{
		Limit: uint32(limit),
	}
	_, err = s.client.Call(
		ctx,
		yarpc.NewReqMeta().Procedure("TaskQueue.Dequeue"),
		request,
		&response,
	)
	if err != nil {
		log.Errorf("Dequeue failed with err=%v", err)
		return nil, err
	}
	return response.Tasks, nil
}

func (s *schedulerManager) getOffers(limit int) (
	offers []*mesos.Offer, err error) {
	// It could happen that the work loop is started before the
	// peloton master inbound is started.  In such case it could
	// panic. This we capture the panic, return error, wait then
	// resume
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Recovered from panic %v", r)
		}
	}()

	ctx, cancelFunc := context.WithTimeout(s.rootCtx, 10*time.Second)
	defer cancelFunc()
	var response offerpool.GetOffersResponse
	var request = &offerpool.GetOffersRequest{
		Limit: uint32(limit),
	}
	_, err = s.client.Call(
		ctx,
		yarpc.NewReqMeta().Procedure("OfferPool.GetOffers"),
		request,
		&response,
	)
	if err != nil {
		log.Errorf("getOffers failed with err=%v", err)
		return nil, err
	}
	return response.Offers, nil
}

// NewMetrics returns a new Metrics struct with all metrics initialized and rooted below the given tally scope
// NOTE: helper function to delegate to metrics.New() to avoid cyclical import dependencies
func NewMetrics(scope tally.Scope) sched_metrics.Metrics {
	return sched_metrics.New(scope)
}
