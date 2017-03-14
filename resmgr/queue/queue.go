package queue

import (
	"errors"
	"peloton/api/respool"
	"peloton/private/resmgr"
)

// Queue is the interface implemented by all the the queues
type Queue interface {
	Enqueue(task *resmgr.Task) error
	Dequeue() (*resmgr.Task, error)
}

// CreateQueue is factory method to create the specified queue
func CreateQueue(policy respool.SchedulingPolicy, limit int64) (Queue, error) {
	// Factory method to create specific queue object based on policy
	switch policy {
	case respool.SchedulingPolicy_PriorityFIFO:
		return NewPriorityQueue(limit), nil
	default:
		//if type is invalid, return an error
		return nil, errors.New("Invalid queue Type")
	}
}