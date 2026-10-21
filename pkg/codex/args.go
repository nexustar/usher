package codex

import (
	"encoding/json"
	"log/slog"
	"strings"
)

// ParseHeadlessArgs maps codex CLI-style flags onto app-server thread params:
// --sandbox and --ask-for-approval become thread params, -c key=value a
// config override. Anything else has no headless equivalent and is dropped
// with a warning.
//
// --ask-for-approval matters for restricted profiles: threadParams hardcodes
// approvalPolicy "on-request", under which a read-only sandbox is only as
// strong as the approval gate — the model can request escalation and an
// auto-approve profile would grant it. "never" closes that; a -c override
// cannot, because thread params take precedence over config.
func ParseHeadlessArgs(args []string, logger *slog.Logger) (sandbox, config map[string]any) {
	if logger == nil {
		logger = slog.Default()
	}
	sandbox, config = map[string]any{}, map[string]any{}
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--sandbox" || args[i] == "-s") && i+1 < len(args):
			sandbox["sandbox"] = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--sandbox="):
			sandbox["sandbox"] = strings.TrimPrefix(args[i], "--sandbox=")
		case (args[i] == "--ask-for-approval" || args[i] == "-a") && i+1 < len(args):
			sandbox["approvalPolicy"] = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--ask-for-approval="):
			sandbox["approvalPolicy"] = strings.TrimPrefix(args[i], "--ask-for-approval=")
		case args[i] == "-c" && i+1 < len(args):
			kv := strings.SplitN(args[i+1], "=", 2)
			if len(kv) == 2 {
				config[kv[0]] = configValue(kv[1])
			} else {
				logger.Warn("invalid -c override", "value", args[i+1])
			}
			i++
		default:
			logger.Warn("unsupported extra codex option", "option", args[i])
		}
	}
	return sandbox, config
}

// Codex's common TOML literals (strings, booleans, numbers and arrays) are
// also valid JSON. Preserve bare TOML words as strings.
func configValue(raw string) any {
	var v any
	if json.Unmarshal([]byte(raw), &v) == nil {
		return v
	}
	return raw
}
