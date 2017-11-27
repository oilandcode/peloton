package batch

import (
	log "github.com/sirupsen/logrus"

	"code.uber.internal/infra/peloton/.gen/mesos/v1"
	"code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc"
	"code.uber.internal/infra/peloton/hostmgr/scalar"
	"code.uber.internal/infra/peloton/placement/models"
	"code.uber.internal/infra/peloton/placement/plugins"
)

// New creates a new batch placement strategy.
func New() plugins.Strategy {
	return &batch{}
}

// batch is the batch placement strategy which just fills up offers with tasks one at a time.
type batch struct{}

func (batch *batch) availablePorts(resources []*mesos_v1.Resource) uint64 {
	var ports uint64
	for _, resource := range resources {
		if resource.GetName() != "ports" {
			continue
		}
		for _, portRange := range resource.GetRanges().GetRange() {
			ports += portRange.GetEnd() - portRange.GetBegin() + 1
		}
	}
	return ports
}

// fillOffer assigns in sequence as many tasks as possible to the given offer,
// and returns a list of tasks not assigned to the offer.
func (batch *batch) fillOffer(offer *models.Host, unassigned []*models.Assignment) []*models.Assignment {
	remainPorts := batch.availablePorts(offer.Offer().GetResources())
	remain := scalar.FromMesosResources(offer.Offer().GetResources())
	for i, placement := range unassigned {
		resmgrTask := placement.Task().Task()
		usedPorts := uint64(resmgrTask.GetNumPorts())
		if usedPorts > remainPorts {
			log.WithFields(log.Fields{
				"resmgr_task":         resmgrTask,
				"num_available_ports": remainPorts,
			}).Debug("Insufficient ports resources.")
			return unassigned[i:]
		}

		usage := scalar.FromResourceConfig(placement.Task().Task().GetResource())
		trySubtract, ok := remain.TrySubtract(usage)
		if !ok {
			log.WithFields(log.Fields{
				"remain": remain,
				"usage":  usage,
			}).Debug("Insufficient resources remain")
			return unassigned[i:]
		}

		remainPorts -= usedPorts
		remain = trySubtract
		placement.SetHost(offer)
	}
	return nil
}

// PlaceOnce is an implementation of the placement.Strategy interface.
func (batch *batch) PlaceOnce(unassigned []*models.Assignment, hosts []*models.Host) {
	for _, host := range hosts {
		log.WithFields(log.Fields{
			"unassigned": unassigned,
			"hosts":      hosts,
		}).Debug("batch placement before")
		unassigned = batch.fillOffer(host, unassigned)
		log.WithFields(log.Fields{
			"unassigned": unassigned,
			"hosts":      hosts,
		}).Debug("batch placement after")
	}
}

func (batch *batch) getHostFilter(assignment *models.Assignment) *hostsvc.HostFilter {
	result := &hostsvc.HostFilter{
		// HostLimit will be later determined by number of tasks.
		ResourceConstraint: &hostsvc.ResourceConstraint{
			Minimum:  assignment.Task().Task().Resource,
			NumPorts: assignment.Task().Task().NumPorts,
		},
	}
	if constraint := assignment.Task().Task().Constraint; constraint != nil {
		result.SchedulingConstraint = constraint
	}
	return result
}

// Filters is an implementation of the placement.Strategy interface.
func (batch *batch) Filters(assignments []*models.Assignment) map[*hostsvc.HostFilter][]*models.Assignment {
	groups := map[string]*hostsvc.HostFilter{}
	filters := map[*hostsvc.HostFilter][]*models.Assignment{}
	for _, assignment := range assignments {
		filter := batch.getHostFilter(assignment)
		// String() function on protobuf message should be nil-safe.
		s := filter.String()
		if _, exists := groups[s]; !exists {
			groups[s] = filter
		}
		batch := filters[groups[s]]
		batch = append(batch, assignment)
		filters[groups[s]] = batch
	}
	return filters
}

// ConcurrencySafe is an implementation of the placement.Strategy interface.
func (batch *batch) ConcurrencySafe() bool {
	return true
}