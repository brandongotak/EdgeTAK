package federation

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
)

// gRPC plumbing for FederatedChannel without generated code: a codec for the
// hand-written messages, the service description for the listener, and
// helpers for the client side.

const service = "com.atakmap.FederatedChannel"

// codec marshals the fig messages. It is named "proto" so the wire content
// type is plain application/grpc, as TAK Server expects.
type codec struct{}

func (codec) Name() string { return "proto" }

func (codec) Marshal(v any) ([]byte, error) {
	m, ok := v.(message)
	if !ok {
		return nil, fmt.Errorf("federation: cannot marshal %T", v)
	}
	return m.marshal(nil), nil
}

func (codec) Unmarshal(data []byte, v any) error {
	m, ok := v.(message)
	if !ok {
		return fmt.Errorf("federation: cannot unmarshal into %T", v)
	}
	return m.unmarshal(data)
}

// handler is what the listener implements; see server.go.
type handler interface {
	sendOneEvent(ctx context.Context, ev *FederatedEvent) error
	getIdentity(ctx context.Context) (*Identity, error)
	healthCheck(ctx context.Context) (*Health, error)
	getAuthToken(ctx context.Context) (*TokenResponse, error)
	getX509Identity(ctx context.Context) (*BinaryBlob, error)
	clientEventStream(sub *Subscription, stream grpc.ServerStream) error
	serverEventStream(stream grpc.ServerStream) error
	serverGroupsStream(sub *Subscription, stream grpc.ServerStream) error
	clientGroupsStream(stream grpc.ServerStream) error
	drainStream(stream grpc.ServerStream, reply message) error // ROL and binary streams: read and discard
	idleStream(stream grpc.ServerStream) error                 // ClientROLStream: open, sends nothing
}

func unary[Req any, PReq interface {
	*Req
	message
}](call func(h handler, ctx context.Context, req PReq) (message, error)) grpc.MethodHandler {
	return func(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
		req := PReq(new(Req))
		if err := dec(req); err != nil {
			return nil, err
		}
		return call(srv.(handler), ctx, req)
	}
}

// serverSubscription reads the Subscription that opens a server stream.
func serverSubscription(stream grpc.ServerStream) (*Subscription, error) {
	var sub Subscription
	if err := stream.RecvMsg(&sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

var serviceDesc = grpc.ServiceDesc{
	ServiceName: service,
	HandlerType: (*handler)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "SendOneEvent", Handler: unary(func(h handler, ctx context.Context, req *FederatedEvent) (message, error) {
			return &Empty{}, h.sendOneEvent(ctx, req)
		})},
		{MethodName: "SendOneBlob", Handler: unary(func(h handler, ctx context.Context, _ *Raw) (message, error) {
			_, err := h.healthCheck(ctx) // authorizes the caller
			return &Empty{}, err
		})},
		{MethodName: "getIdentity", Handler: unary(func(h handler, ctx context.Context, _ *Empty) (message, error) {
			return h.getIdentity(ctx)
		})},
		{MethodName: "HealthCheck", Handler: unary(func(h handler, ctx context.Context, _ *Health) (message, error) {
			return h.healthCheck(ctx)
		})},
		{MethodName: "GetAuthTokenByX509", Handler: unary(func(h handler, ctx context.Context, _ *Raw) (message, error) {
			return h.getAuthToken(ctx)
		})},
		{MethodName: "Getx509Identity", Handler: unary(func(h handler, ctx context.Context, _ *Empty) (message, error) {
			return h.getX509Identity(ctx)
		})},
	},
	Streams: []grpc.StreamDesc{
		{StreamName: "ClientEventStream", ServerStreams: true, Handler: func(srv any, stream grpc.ServerStream) error {
			sub, err := serverSubscription(stream)
			if err != nil {
				return err
			}
			return srv.(handler).clientEventStream(sub, stream)
		}},
		{StreamName: "ServerEventStream", ClientStreams: true, Handler: func(srv any, stream grpc.ServerStream) error {
			return srv.(handler).serverEventStream(stream)
		}},
		{StreamName: "ServerFederateGroupsStream", ServerStreams: true, Handler: func(srv any, stream grpc.ServerStream) error {
			sub, err := serverSubscription(stream)
			if err != nil {
				return err
			}
			return srv.(handler).serverGroupsStream(sub, stream)
		}},
		{StreamName: "ClientFederateGroupsStream", ClientStreams: true, Handler: func(srv any, stream grpc.ServerStream) error {
			return srv.(handler).clientGroupsStream(stream)
		}},
		{StreamName: "ClientROLStream", ServerStreams: true, Handler: func(srv any, stream grpc.ServerStream) error {
			if _, err := serverSubscription(stream); err != nil {
				return err
			}
			return srv.(handler).idleStream(stream)
		}},
		{StreamName: "ServerROLStream", ClientStreams: true, Handler: func(srv any, stream grpc.ServerStream) error {
			return srv.(handler).drainStream(stream, &Subscription{})
		}},
		{StreamName: "BinaryMessageStream", ClientStreams: true, Handler: func(srv any, stream grpc.ServerStream) error {
			return srv.(handler).drainStream(stream, &Empty{})
		}},
	},
	Metadata: "fig.proto",
}

// ---- client side ----

func method(name string) string { return "/" + service + "/" + name }

var (
	clientStreamDesc = &grpc.StreamDesc{ClientStreams: true}
	serverStreamDesc = &grpc.StreamDesc{ServerStreams: true}
)

// openServerStream starts an RPC that sends one request and receives a stream.
func openServerStream(ctx context.Context, cc *grpc.ClientConn, name string, req message) (grpc.ClientStream, error) {
	st, err := cc.NewStream(ctx, serverStreamDesc, method(name), grpc.ForceCodec(codec{}))
	if err != nil {
		return nil, err
	}
	if err := st.SendMsg(req); err != nil {
		return nil, err
	}
	return st, st.CloseSend()
}

// openClientStream starts an RPC that sends a stream and receives one reply.
func openClientStream(ctx context.Context, cc *grpc.ClientConn, name string) (grpc.ClientStream, error) {
	return cc.NewStream(ctx, clientStreamDesc, method(name), grpc.ForceCodec(codec{}))
}

func invoke(ctx context.Context, cc *grpc.ClientConn, name string, req, reply message) error {
	return cc.Invoke(ctx, method(name), req, reply, grpc.ForceCodec(codec{}))
}
