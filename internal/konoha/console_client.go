package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"prreview/internal/konoha/consoleapi"
)

// #124: the CLI's console HTTP client, generated from api/openapi.yaml. This is
// the single place the CLI talks to the console API over HTTP — no ad-hoc
// requests/JSON shapes. Auth is the bearer token; a CLI/worker mutation presents
// the bearer token instead of the browser CSRF token.
//
// Usage: `prbot api <status|metrics|cycles|cycle <id>|artifacts <id>|queue>`
// Base URL from KONOHA_CONSOLE_ADDR (loopback), token from KONOHA_CONSOLE_TOKEN.

func runConsoleAPI(args []string) error {
	payload, err := consoleAPIPayload(args)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// consoleAPIPayload performs one generated-client request and returns the typed
// response payload (the test seam — it needs no stdout capture).
func consoleAPIPayload(args []string) (any, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("usage: prbot api <status|metrics|cycles|cycle <id>|artifacts <id>|queue>")
	}
	base, token, err := consoleAPIConn()
	if err != nil {
		return nil, err
	}
	client, err := consoleapi.NewClientWithResponses(base, consoleapi.WithRequestEditorFn(
		func(_ context.Context, req *http.Request) error {
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			return nil
		}))
	if err != nil {
		return nil, err
	}
	ctx := context.Background()

	switch args[0] {
	case "status":
		r, err := client.GetStatusWithResponse(ctx)
		if err != nil {
			return nil, err
		}
		if r.JSON200 == nil {
			return nil, consoleAPIStatusErr("status", r.StatusCode(), r.Body)
		}
		return r.JSON200, nil
	case "metrics":
		r, err := client.GetMetricsWithResponse(ctx)
		if err != nil {
			return nil, err
		}
		if r.JSON200 == nil {
			return nil, consoleAPIStatusErr("metrics", r.StatusCode(), r.Body)
		}
		return r.JSON200, nil
	case "cycles":
		r, err := client.ListCyclesWithResponse(ctx)
		if err != nil {
			return nil, err
		}
		if r.JSON200 == nil {
			return nil, consoleAPIStatusErr("cycles", r.StatusCode(), r.Body)
		}
		return r.JSON200, nil
	case "queue":
		r, err := client.GetQueueWithResponse(ctx)
		if err != nil {
			return nil, err
		}
		if r.JSON200 == nil {
			return nil, consoleAPIStatusErr("queue", r.StatusCode(), r.Body)
		}
		return r.JSON200, nil
	case "cycle":
		id, err := consoleAPIArg(args, "cycle <id>")
		if err != nil {
			return nil, err
		}
		r, err := client.GetCycleWithResponse(ctx, id)
		if err != nil {
			return nil, err
		}
		if r.JSON200 == nil {
			return nil, consoleAPIStatusErr("cycle", r.StatusCode(), r.Body)
		}
		return r.JSON200, nil
	case "artifacts":
		id, err := consoleAPIArg(args, "artifacts <id>")
		if err != nil {
			return nil, err
		}
		r, err := client.ListArtifactsWithResponse(ctx, id)
		if err != nil {
			return nil, err
		}
		if r.JSON200 == nil {
			return nil, consoleAPIStatusErr("artifacts", r.StatusCode(), r.Body)
		}
		return r.JSON200, nil
	default:
		return nil, fmt.Errorf("unknown api resource %q", args[0])
	}
}

// consoleAPIArg returns the required positional argument for a subcommand.
func consoleAPIArg(args []string, usage string) (string, error) {
	if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
		return "", fmt.Errorf("usage: prbot api %s", usage)
	}
	return args[1], nil
}

// consoleAPIConn resolves the console base URL + bearer token. The address must
// be loopback (the console never binds a routable host).
func consoleAPIConn() (base, token string, err error) {
	addr := strings.TrimSpace(os.Getenv(consoleAddrEnv))
	if addr == "" {
		return "", "", fmt.Errorf("api: %s is not set", consoleAddrEnv)
	}
	if err := validateLoopbackAddr(addr); err != nil {
		return "", "", err
	}
	return "http://" + addr, os.Getenv(consoleTokenEnv), nil
}

// consoleAPIStatusErr renders a non-2xx response from the generated client.
func consoleAPIStatusErr(what string, code int, body []byte) error {
	return fmt.Errorf("api %s: HTTP %d: %s", what, code, strings.TrimSpace(string(body)))
}
