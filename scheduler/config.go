package scheduler

// Peloton scheduler specific configuration
type Config struct {
	// Max number of tasks to dequeue in a request
	TaskDequeueLimit int `yaml:"task_dequeue_limit"`
}