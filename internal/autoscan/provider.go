package autoscan

import (
	"context"
	"errors"
	"regexp"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ScanSourceProvider yields changed paths for one source. The engine calls
// PollChanges; production wraps the plugins.Service scan_source resolver.
type ScanSourceProvider interface {
	PollChanges(ctx context.Context, pluginID, capabilityID, marker string, conn ResolvedConnection, sourceConfig map[string]string) (changes []Change, nextMarker string, err error)
}

// PollChangesClient is the slice of *pluginhost.ScanSourceClient used here. It
// is exported so wiring in the api package can declare an adapter whose
// ScanSourceClient returns it (Go interface method signatures must match
// exactly across packages, and the unexported form could not be named there).
type PollChangesClient interface {
	PollChanges(ctx context.Context, req *pluginv1.PollChangesRequest) (*pluginv1.PollChangesResponse, error)
}

// ScanSourceResolver yields a per-(plugin, capability) scan-source client.
// Exported for the same cross-package adapter reason as PollChangesClient.
type ScanSourceResolver interface {
	ScanSourceClient(ctx context.Context, pluginID, capabilityID string) (PollChangesClient, error)
}

type pluginProvider struct{ resolver ScanSourceResolver }

// NewPluginProvider builds the production scan-source provider over the plugins
// resolver.
func NewPluginProvider(resolver ScanSourceResolver) ScanSourceProvider {
	return &pluginProvider{resolver: resolver}
}

func (p *pluginProvider) PollChanges(ctx context.Context, pluginID, capabilityID, marker string, conn ResolvedConnection, sourceConfig map[string]string) ([]Change, string, error) {
	client, err := p.resolver.ScanSourceClient(ctx, pluginID, capabilityID)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.PollChanges(ctx, &pluginv1.PollChangesRequest{
		CapabilityId: capabilityID,
		Marker:       marker,
		Connection:   &pluginv1.ResolvedConnection{BaseUrl: conn.BaseURL, ApiKey: conn.APIKey},
		SourceConfig: sourceConfig,
	})
	if err != nil {
		return nil, "", err
	}
	if structured := resp.GetChanges(); len(structured) > 0 {
		changes := make([]Change, 0, len(structured))
		for _, change := range structured {
			if change == nil {
				continue
			}
			changes = append(changes, Change{
				SourcePath: change.GetSourcePath(),
				Scope:      scanSourceScope(change.GetScope()),
			})
		}
		return changes, resp.GetNextMarker(), nil
	}

	// Legacy plugins return only source_paths. Treat them as auto/file-like
	// paths so the existing parent-directory collapse behavior is preserved.
	changes := make([]Change, 0, len(resp.GetSourcePaths()))
	for _, path := range resp.GetSourcePaths() {
		changes = append(changes, Change{SourcePath: path, Scope: ChangeScopeAuto})
	}
	return changes, resp.GetNextMarker(), nil
}

func scanSourceScope(scope pluginv1.ScanSourceChangeScope) ChangeScope {
	switch scope {
	case pluginv1.ScanSourceChangeScope_SCAN_SOURCE_CHANGE_SCOPE_FILE:
		return ChangeScopeFile
	case pluginv1.ScanSourceChangeScope_SCAN_SOURCE_CHANGE_SCOPE_SUBTREE:
		return ChangeScopeSubtree
	default:
		return ChangeScopeAuto
	}
}

// pollErrorMessage turns a provider failure into the text the host stores on
// the source (last_error) and the poll event (error_message), which the admin
// UI shows verbatim.
//
// Plugin errors cross the go-plugin gRPC transport, so a plain
// fmt.Errorf("...") inside a plugin arrives as
// "rpc error: code = Unknown desc = ...". The framing tells an operator
// nothing; the plugin's own description is the useful part. Codes the
// transport itself produces (the plugin process is gone, the call timed out)
// carry no plugin text worth showing, so they get a short host-written
// explanation instead.
const pollTimedOutMessage = "Plugin timed out."

var rpcErrorFraming = regexp.MustCompile(`rpc error: code = [A-Za-z]+ desc = `)

func pollErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	st, ok := status.FromError(err)
	if !ok {
		if errors.Is(err, context.DeadlineExceeded) {
			return pollTimedOutMessage
		}
		return err.Error()
	}
	// A status wrapped by host code reports the whole chain as its message,
	// framing included, so strip the framing wherever it appears.
	desc := strings.TrimSpace(rpcErrorFraming.ReplaceAllString(st.Message(), ""))
	switch st.Code() {
	case codes.Unavailable:
		if desc == "" {
			return "Plugin unavailable."
		}
		return "Plugin unavailable: " + desc
	case codes.DeadlineExceeded:
		return pollTimedOutMessage
	case codes.Canceled:
		return "Poll canceled."
	case codes.Unimplemented:
		return "Plugin does not support polling for changes."
	}
	if desc == "" {
		return "Plugin error: " + st.Code().String()
	}
	return desc
}
