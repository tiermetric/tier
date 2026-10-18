package collector

import "log/slog"

// LogRevertBranchEvents counts only events retained by this collector pass.
func LogRevertBranchEvents(logger *slog.Logger, events []TokenEvent) {
	count := 0
	for _, event := range events {
		if event.RevertBranch {
			count++
		}
	}
	if count == 0 {
		return
	}
	logger.Info("revert-branch events left unattributed", "count", count)
}
