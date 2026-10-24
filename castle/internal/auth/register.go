package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/store"
)

// Wire-carried registration metadata (KB-318). The criteria.v1 wire contract
// is an exact mirror of the released criteria/sdk and must not gain castle
// fields, so the agent identity block (User-Agent pattern) and the project
// the agent joins at registration ride castle-side connect metadata headers
// on the unmodified Register RPC. Header names are stable configuration
// surface: agents set them, castle parses and persists them verbatim.
const (
	// HeaderAgentProject carries the project id the agent self-joins at
	// registration (OPEN-policy self-join; CLOSED projects refuse it).
	HeaderAgentProject = "x-criteria-agent-project"
	// HeaderAgentVersion is the agent's build version.
	HeaderAgentVersion = "x-criteria-agent-version"
	// HeaderAgentRepoURL is the source repository the agent was built from.
	HeaderAgentRepoURL = "x-criteria-agent-repo-url"
	// HeaderAgentBuildDate is the agent's build date (opaque string).
	HeaderAgentBuildDate = "x-criteria-agent-build-date"
	// HeaderAgentSDKVersion carries one component version as "name:version";
	// the header may repeat (e.g. criteria and adapter SDK versions).
	HeaderAgentSDKVersion = "x-criteria-agent-sdk-version"
	// HeaderAgentExtra carries one opaque info entry as "key:value"; the
	// header may repeat.
	HeaderAgentExtra = "x-criteria-agent-extra"
)

// Identity block wire limits: registration metadata is persisted verbatim, so
// the interceptor bounds it before any handler or storage sees it. These caps
// keep a single Register call's identity payload small; larger blocks are a
// client misconfiguration, not a server problem.
const (
	identityHeaderMax       = 512  // bytes per single-value identity header
	identitySDKVersionMax   = 32   // repeated sdk-version entries
	identitySDKVersionField = 256  // bytes per sdk name or version
	identityExtraMax        = 64   // repeated extra entries
	identityExtraKeyMax     = 128  // bytes per extra key
	identityExtraValueMax   = 1024 // bytes per extra value
)

// IdentityEchoLabel is the reserved agent label key the server reads back the
// persisted identity block through in ListAgents/GetAgent responses (the
// criteria.v1 Agent type is an exact upstream mirror, so the echo rides the
// existing labels map). An agent-supplied label with the same name is
// overwritten by the server's own record whenever an identity block exists.
const IdentityEchoLabel = "identity"

// registrationIdentityKey is the context key for the parsed agent identity
// block presented on Register metadata.
type registrationIdentityKey struct{}

// registrationProjectIDKey is the context key for the project id an agent
// self-joins at registration.
type registrationProjectIDKey struct{}

// AgentIdentityFromContext returns the agent identity block the interceptor
// parsed from Register metadata, or nil when none was presented.
func AgentIdentityFromContext(ctx context.Context) *store.AgentIdentity {
	v, _ := ctx.Value(registrationIdentityKey{}).(*store.AgentIdentity)
	return v
}

// WithAgentIdentity returns a context carrying the parsed agent identity
// block. Use in tests that call handlers directly (no HTTP stack).
func WithAgentIdentity(ctx context.Context, i *store.AgentIdentity) context.Context {
	return context.WithValue(ctx, registrationIdentityKey{}, i)
}

// RegisterProjectIDFromContext returns the project id the registering agent
// self-joins, or "" when no project metadata was presented.
func RegisterProjectIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(registrationProjectIDKey{}).(string)
	return v
}

// WithRegisterProjectID returns a context carrying the registration project
// id. Use in tests that call handlers directly (no HTTP stack).
func WithRegisterProjectID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, registrationProjectIDKey{}, id)
}

// ParseAgentRegistration parses the agent identity block and project id from
// Register metadata. Malformed or oversized metadata yields a connect error
// (InvalidArgument) so bad client configuration fails fast at registration;
// absence of the headers is valid (no block, no join). The returned identity
// is non-nil even when every field is absent; handlers treat the zero value
// as "no block".
func ParseAgentRegistration(header http.Header) (*store.AgentIdentity, string, error) {
	identity := &store.AgentIdentity{}

	single := func(name string) (string, error) {
		// http.Header keys are MIME-canonical ("X-Criteria-Agent-Version"),
		// so lookups must canonicalize too; lowercase constants stay stable
		// public surface for agents.
		values := header.Values(name)
		switch {
		case len(values) > 1:
			return "", invalidArg("%s must not repeat", name)
		case len(values) == 1 && len(values[0]) > identityHeaderMax:
			return "", invalidArg("%s exceeds %d bytes", name, identityHeaderMax)
		case len(values) == 1:
			return values[0], nil
		default:
			return "", nil
		}
	}

	var err error
	if identity.Version, err = single(HeaderAgentVersion); err != nil {
		return nil, "", err
	}
	if identity.RepoURL, err = single(HeaderAgentRepoURL); err != nil {
		return nil, "", err
	}
	if identity.BuildDate, err = single(HeaderAgentBuildDate); err != nil {
		return nil, "", err
	}

	if len(header.Values(HeaderAgentSDKVersion)) > identitySDKVersionMax {
		return nil, "", invalidArg("%s exceeds %d entries", HeaderAgentSDKVersion, identitySDKVersionMax)
	}
	for _, v := range header.Values(HeaderAgentSDKVersion) {
		name, version, err := splitPair(v, identitySDKVersionField, identitySDKVersionField, "sdk version")
		if err != nil {
			return nil, "", err
		}
		identity.SDKVersions = append(identity.SDKVersions, store.AgentSDKVersion{Name: name, Version: version})
	}

	if len(header.Values(HeaderAgentExtra)) > identityExtraMax {
		return nil, "", invalidArg("%s exceeds %d entries", HeaderAgentExtra, identityExtraMax)
	}
	if len(header.Values(HeaderAgentExtra)) > 0 {
		identity.Extra = make(map[string]string, len(header.Values(HeaderAgentExtra)))
		for _, v := range header.Values(HeaderAgentExtra) {
			key, value, err := splitPair(v, identityExtraKeyMax, identityExtraValueMax, "extra")
			if err != nil {
				return nil, "", err
			}
			identity.Extra[key] = value
		}
	}

	projectID, err := single(HeaderAgentProject)
	if err != nil {
		return nil, "", err
	}
	if projectID != "" {
		// Project ids are slugs; validate the shape here so a malformed
		// header is a 4xx at registration instead of a store error surfacing
		// mid-join.
		if err := store.ValidateProjectID(projectID); err != nil {
			return nil, "", connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	return identity, projectID, nil
}

// invalidArg builds the InvalidArgument connect error registration metadata
// problems surface as.
func invalidArg(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// splitPair splits a "name:value" metadata entry at the first colon and
// enforces per-field byte caps. A missing separator is an InvalidArgument.
func splitPair(v string, keyMax, valueMax int, what string) (string, string, error) {
	name, value, ok := strings.Cut(v, ":")
	if !ok {
		return "", "", invalidArg("%s entry %q is not name:value", what, v)
	}
	if len(name) == 0 || len(name) > keyMax {
		return "", "", invalidArg("%s name %q must be 1..%d bytes", what, name, keyMax)
	}
	if len(value) > valueMax {
		return "", "", invalidArg("%s value %q exceeds %d bytes", what, name, valueMax)
	}
	return name, value, nil
}
