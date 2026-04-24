package auth

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// LoggingInterceptor emits structured request logs for Connect handlers.
type LoggingInterceptor struct {
	log *slog.Logger
}

func NewLoggingInterceptor(log *slog.Logger) *LoggingInterceptor {
	if log == nil {
		log = slog.Default()
	}
	return &LoggingInterceptor{log: log}
}

func (i *LoggingInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		i.logRequest("unary", req.Spec().Procedure, peerFromReq(req), connect.CodeOf(err), time.Since(start), runIDFromMessage(req.Any()))
		return resp, err
	}
}

func (i *LoggingInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *LoggingInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		err := next(ctx, conn)
		i.logRequest("stream", conn.Spec().Procedure, peerFromStream(conn), connect.CodeOf(err), time.Since(start), "")
		return err
	}
}

func (i *LoggingInterceptor) logRequest(rpcType, method, peer string, code connect.Code, d time.Duration, runID string) {
	args := []any{
		"rpc", rpcType,
		"method", method,
		"peer", peer,
		"duration_ms", d.Milliseconds(),
		"code", strings.ToLower(code.String()),
	}
	if runID != "" {
		args = append(args, "run_id", runID)
	}
	i.log.Info("rpc request", args...)
}

func peerFromReq(req connect.AnyRequest) string {
	p := req.Peer()
	if p.Addr != "" {
		return p.Addr
	}
	return ""
}

func peerFromStream(conn connect.StreamingHandlerConn) string {
	p := conn.Peer()
	if p.Addr != "" {
		return p.Addr
	}
	return ""
}

func runIDFromMessage(msg any) string {
	pm, ok := msg.(proto.Message)
	if !ok || pm == nil {
		return ""
	}
	m := pm.ProtoReflect()
	if !m.IsValid() {
		return ""
	}
	fd := m.Descriptor().Fields().ByName(protoreflect.Name("run_id"))
	if fd == nil || !m.Has(fd) {
		return ""
	}
	v := m.Get(fd)
	switch fd.Kind() {
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.Uint64Kind:
		return strconv.FormatUint(uint64(v.Uint()), 10)
	default:
		return ""
	}
}
