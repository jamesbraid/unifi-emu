package herder

import "github.com/testcontainers/testcontainers-go"

// CheckEffectiveReaper refuses to start devices when the stock Testcontainers
// reaper is disabled.
//
// Crash cleanup is the only thing that removes device containers when the
// herder is killed, and it is the session's job, not the labels'. A harness
// that exports TESTCONTAINERS_RYUK_DISABLED for its own Compose lifecycle
// would otherwise silently downgrade this run into one that leaks containers
// on SIGKILL. The check reads effective behaviour -- environment and
// properties file -- through the library's own public configuration, so
// inherited process state cannot slip past it.
func CheckEffectiveReaper() error {
	return checkReaper(testcontainers.ReadConfig())
}

func checkReaper(cfg testcontainers.TestcontainersConfig) error {
	if cfg.Config.RyukDisabled {
		return failf(CodeReaperDisabled, PhaseValidate,
			"the effective Testcontainers configuration disables the reaper")
	}
	return nil
}
