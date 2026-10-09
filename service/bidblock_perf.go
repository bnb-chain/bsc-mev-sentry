package service

import (
	"context"
	"encoding/json"
	"time"

	"google.golang.org/grpc"
)

// bidBlockJSONArgs has the same wire shape, without UnmarshalJSON recursion.
type bidBlockJSONArgs BidBlockArgsWrapper

// UnmarshalJSON times argument decoding, which runs before SendBidBlock is entered.
// The JSON-RPC envelope and the outer JSON scanner are not included.
func (a *BidBlockArgsWrapper) UnmarshalJSON(data []byte) error {
	start := time.Now()
	err := json.Unmarshal(data, (*bidBlockJSONArgs)(a))
	a.decodeElapsed = time.Since(start)
	a.payloadBytes = len(data)
	return err
}

type protoDecodeKey struct{}

// withProtoDecodeTiming times request unmarshalling, which grpc-go runs inside
// the method handler before any interceptor sees the request.
// Its decoder also releases receive buffers and invokes any configured stats/
// tracing hooks; this is framework decode time, not an isolated proto benchmark.
// Body reception and decompression occur before the decoder and are excluded.
func withProtoDecodeTiming(desc grpc.ServiceDesc) *grpc.ServiceDesc {
	methods := make([]grpc.MethodDesc, len(desc.Methods))
	for i, m := range desc.Methods {
		handler := m.Handler
		m.Handler = func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			elapsed := new(time.Duration)
			ctx = context.WithValue(ctx, protoDecodeKey{}, elapsed)
			return handler(srv, ctx, func(v any) error {
				start := time.Now()
				err := dec(v)
				*elapsed = time.Since(start)
				return err
			}, interceptor)
		}
		methods[i] = m
	}
	desc.Methods = methods
	return &desc
}

func protoDecodeElapsed(ctx context.Context) time.Duration {
	if elapsed, ok := ctx.Value(protoDecodeKey{}).(*time.Duration); ok {
		return *elapsed
	}
	return 0
}
