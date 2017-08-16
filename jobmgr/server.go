package jobmgr

import (
	"sync"

	"code.uber.internal/infra/peloton/common"
	"code.uber.internal/infra/peloton/jobmgr/task/event"
	"code.uber.internal/infra/peloton/leader"
	log "github.com/sirupsen/logrus"
)

// LeaderLifeCycle implementations is called to follow the leader start and
// stop calles.
type LeaderLifeCycle interface {
	// Start the life cycle as the leadership was gained.
	Start() error
	// Stop the life cycle as the leadership was lost.
	Stop() error
}

// Server contains all structs necessary to run a jobmgr server.
// This struct also implements leader.Node interface so that it can
// perform leader election among multiple job manager server
// instances.
type Server struct {
	sync.Mutex

	ID   string
	role string

	getStatusUpdate   func() event.StatusUpdate
	getStatusUpdateRM func() event.StatusUpdateRM

	llcs []LeaderLifeCycle
}

// NewServer creates a job manager Server instance.
func NewServer(
	httpPort, grpcPort int,
	llcs ...LeaderLifeCycle,
) *Server {

	return &Server{
		ID:                leader.NewID(httpPort, grpcPort),
		role:              common.JobManagerRole,
		getStatusUpdate:   event.GetStatusUpdater,
		getStatusUpdateRM: event.GetStatusUpdaterRM,
		llcs:              llcs,
	}
}

// GainedLeadershipCallback is the callback when the current node
// becomes the leader
func (s *Server) GainedLeadershipCallback() error {

	log.WithFields(log.Fields{"role": s.role}).Info("Gained leadership")

	s.getStatusUpdate().Start()
	s.getStatusUpdateRM().Start()

	for _, l := range s.llcs {
		l.Start()
	}

	return nil
}

// LostLeadershipCallback is the callback when the current node lost
// leadership
func (s *Server) LostLeadershipCallback() error {

	log.WithField("role", s.role).Info("Lost leadership")

	s.getStatusUpdate().Stop()
	s.getStatusUpdateRM().Stop()

	for _, l := range s.llcs {
		l.Stop()
	}

	return nil
}

// ShutDownCallback is the callback to shut down gracefully if possible
func (s *Server) ShutDownCallback() error {

	log.WithFields(log.Fields{"role": s.role}).Info("Quitting election")

	s.getStatusUpdate().Stop()
	s.getStatusUpdateRM().Stop()

	for _, l := range s.llcs {
		l.Stop()
	}

	return nil
}

// GetID function returns jobmgr app address.
// This implements leader.Nomination.
func (s *Server) GetID() string {
	return s.ID
}
