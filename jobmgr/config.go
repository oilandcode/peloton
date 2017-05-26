package jobmgr

import (
	"code.uber.internal/infra/peloton/jobmgr/task/launcher"
)

// Config is JobManager specific configuration
type Config struct {
	Port int `yaml:"port"`
	// FIXME(gabe): this isnt really the DB write concurrency. This is
	// only used for processing task updates and should be moved into
	// the storage namespace, and made clearer what this controls
	// (threads? rows? statements?)
	DbWriteConcurrency int `yaml:"db_write_concurrency"`

	// Task launcher specific configs
	TaskLauncher launcher.Config `yaml:"task_launcher"`
}
