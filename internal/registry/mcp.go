package registry

import (
	"context"
	"errors"
	"net/http"

	"github.com/OpenLinker-ai/openlinker-registry/internal/mcp"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
)

// MCPPublishOptions describe one server.json publish.
type MCPPublishOptions struct {
	Actor     string
	Namespace string
	// Indexed marks the entry as recorded by someone other than the server's
	// author (design §4). It only applies when the server is created.
	Indexed    bool
	Visibility string // optional; only applies when the server is created
	Listed     *bool  // optional; only applies when the server is created
}

// PublishMCP validates a server.json and stores it as a new immutable version
// of a registered (or indexed) MCP server. The namespace part of the server
// name must be the namespace the caller publishes into; that is what ties a
// name to a verified namespace.
func (s *Service) PublishMCP(ctx context.Context, o MCPPublishOptions, body []byte) (model.MCPServer, model.MCPVersion, error) {
	p, err := mcp.Parse(body, mcp.Options{Strict: true, AllowHTTPRemotes: s.Cfg.Mode == "private"})
	if err != nil {
		return model.MCPServer{}, model.MCPVersion{}, err
	}
	if p.Namespace != o.Namespace {
		return model.MCPServer{}, model.MCPVersion{}, fail(http.StatusUnprocessableEntity, "namespace_mismatch",
			"the server name must start with the namespace it is published to ("+o.Namespace+"/…)")
	}
	ns, err := s.Store.GetNamespace(ctx, o.Namespace)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return model.MCPServer{}, model.MCPVersion{}, fail(http.StatusNotFound, "namespace_not_found", "namespace not found")
		}
		return model.MCPServer{}, model.MCPVersion{}, err
	}
	if ns.UpstreamID != 0 {
		return model.MCPServer{}, model.MCPVersion{}, model.ErrNamespaceManaged()
	}
	mode, action := model.MCPModeRegistered, "mcp.publish"
	if o.Indexed {
		mode, action = model.MCPModeIndexed, "mcp.index"
	}
	visibility := o.Visibility
	if existing, err := s.Store.GetMCPServer(ctx, p.Name); err == nil {
		visibility = existing.Visibility
	} else if !errors.Is(err, model.ErrNotFound) {
		return model.MCPServer{}, model.MCPVersion{}, err
	} else {
		if visibility == "" {
			visibility = s.DefaultVisibility()
		}
		if err := s.checkVisibility(visibility); err != nil {
			return model.MCPServer{}, model.MCPVersion{}, err
		}
	}
	listed := true
	if o.Listed != nil {
		listed = *o.Listed
	}
	return s.Store.PublishMCPVersion(ctx, model.MCPPublishInput{
		Namespace: o.Namespace, Name: p.Name, Mode: mode, Visibility: visibility, Listed: listed,
		Version: model.MCPVersion{Version: p.Version, ServerJSON: p.Canonical, Digest: p.Digest, CreatedBy: o.Actor},
	}, auditEvent(o.Actor, action, o.Namespace, p.Name+"@"+p.Version, map[string]any{"digest": p.Digest}))
}

func mirroredReadOnly() error {
	return fail(http.StatusConflict, "mirrored_read_only", "this entry is mirrored from an upstream and cannot be changed here")
}

// SetMCPStatus changes the status of one version (or of every version when
// version is ""). It returns the number of versions changed.
func (s *Service) SetMCPStatus(ctx context.Context, actor string, srv model.MCPServer, version, status, message string) (int, error) {
	if srv.Mode == model.MCPModeMirrored {
		return 0, mirroredReadOnly()
	}
	if !model.ValidMCPStatus(status) {
		return 0, fail(http.StatusUnprocessableEntity, "status_invalid", "status must be active, deprecated or deleted")
	}
	if len(message) > model.MaxMCPStatusMessage {
		return 0, fail(http.StatusUnprocessableEntity, "status_message_too_long", "statusMessage must be at most 500 bytes")
	}
	if status == model.MCPStatusActive && message != "" {
		return 0, fail(http.StatusUnprocessableEntity, "status_message_not_allowed", "statusMessage is not allowed when status is active")
	}
	target := srv.Name
	if version != "" {
		target += "@" + version
	}
	return s.Store.SetMCPStatus(ctx, srv.ID, version, status, message, s.Now(),
		auditEvent(actor, "mcp.status", srv.Namespace, target, map[string]any{"status": status, "message": message}))
}

// UpdateMCPSettings changes visibility or listing of a local MCP server.
func (s *Service) UpdateMCPSettings(ctx context.Context, actor string, srv model.MCPServer, p model.MCPPatch) (model.MCPServer, error) {
	if srv.Mode == model.MCPModeMirrored {
		return model.MCPServer{}, mirroredReadOnly()
	}
	details := map[string]any{}
	if p.Visibility != nil {
		if err := s.checkVisibility(*p.Visibility); err != nil {
			return model.MCPServer{}, err
		}
		details["visibility"] = *p.Visibility
	}
	if p.Listed != nil {
		details["listed"] = *p.Listed
	}
	return s.Store.UpdateMCPServer(ctx, srv.ID, p, auditEvent(actor, "mcp.update", srv.Namespace, srv.Name, details))
}

// ResolveMCP returns a server and one of its versions ("latest" allowed). On a
// local miss it forwards the read to the upstreams that allow it (see
// forwardMCP) and mirrors the answer. Visibility is the caller's concern.
func (s *Service) ResolveMCP(ctx context.Context, viewer model.Viewer, name, version string, forward bool) (model.MCPServer, model.MCPVersion, error) {
	srv, err := s.Store.GetMCPServer(ctx, name)
	if err == nil {
		v, verr := s.Store.GetMCPVersion(ctx, srv.ID, version)
		if verr == nil || !errors.Is(verr, model.ErrNotFound) {
			return srv, v, verr
		}
		// A mirrored server may have versions upstream that were not synced.
		if srv.Mode != model.MCPModeMirrored || !forward {
			return srv, v, model.ErrNotFound
		}
	} else if !errors.Is(err, model.ErrNotFound) || !forward {
		return srv, model.MCPVersion{}, err
	}
	if err := s.forwardMCP(ctx, viewer, name, version); err != nil {
		return model.MCPServer{}, model.MCPVersion{}, err
	}
	srv, err = s.Store.GetMCPServer(ctx, name)
	if err != nil {
		return srv, model.MCPVersion{}, err
	}
	v, err := s.Store.GetMCPVersion(ctx, srv.ID, version)
	return srv, v, err
}
