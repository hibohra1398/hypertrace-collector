package ratelimiter

import (
	"context"
	"fmt"
	"strings"

	pb_struct "github.com/envoyproxy/go-control-plane/envoy/extensions/common/ratelimit/v3"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

var _ processor.Traces = (*rateLimiterProcessor)(nil)

func (p *rateLimiterProcessor) Start(_ context.Context, _ component.Host) error {
	return nil
}

func (p *rateLimiterProcessor) Shutdown(_ context.Context) error {
	err := p.rateLimitServiceClientConn.Close()
	if err != nil {
		p.logger.Error("failure while closing rate limit service client connection ", zap.Error(err))
	}
	p.cancelFunc()
	return nil
}

func (p *rateLimiterProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: true}
}

type rateLimiterProcessor struct {
	rateLimitServiceClient     pb.RateLimitServiceClient
	domain                     string
	logger                     *zap.Logger
	tenantIDHeaderName         string
	nextConsumer               consumer.Traces
	rateLimitServiceClientConn *grpc.ClientConn
	cancelFunc                 context.CancelFunc
}

const (
	TenantSpans = "tenant_spans"
)

// ConsumeTraces consume traces and drops the requests if it is rate limited,
// otherwise calls next consumer
func (p *rateLimiterProcessor) ConsumeTraces(ctx context.Context, traces ptrace.Traces) error {
	tenantId, err := p.getTenantId(ctx)
	if err != nil {
		// If tenantId is missing, rate limiting not applicable.
		p.logger.Error("unable to extract tenantId ", zap.Error(err))
		return p.nextConsumer.ConsumeTraces(ctx, traces)
	}
	desc := make([]*pb_struct.RateLimitDescriptor, 1)
	desc[0] = &pb_struct.RateLimitDescriptor{
		Entries: []*pb_struct.RateLimitDescriptor_Entry{
			{
				Key:   TenantSpans,
				Value: tenantId,
			},
		},
	}
	ctx, _ = tag.New(ctx,
		tag.Insert(tagTenantID, tenantId))
	// G115 (CWE-190): integer overflow conversion int -> uint32 (Confidence: MEDIUM, Severity: HIGH)
	// This is a false positive we can ignore.
	spanCount := uint32(traces.SpanCount()) // #nosec G115
	stats.Record(ctx, rateLimitServiceCallsCount.M(int64(1)))
	response, err := p.rateLimitServiceClient.ShouldRateLimit(
		ctx,
		&pb.RateLimitRequest{
			Domain:      p.domain,
			Descriptors: desc,
			HitsAddend:  spanCount,
		})
	if err != nil {
		// Rate limit service call fails, spans will be forwarded as it is.
		p.logger.Error("rate limit service call failed", zap.Error(err))
		return p.nextConsumer.ConsumeTraces(ctx, traces)
	}
	descriptorStatuses := response.Statuses
	if len(descriptorStatuses) == 1 {
		if descriptorStatuses[0].GetCode() == pb.RateLimitResponse_OVER_LIMIT {
			// If tenant rate limit exceeded drop request.
			p.logger.Warn(fmt.Sprintf("dropping spans for tenant %s as rate limit exceeded, of spancount: %d", tenantId, spanCount))
			stats.Record(ctx, droppedSpanCount.M(int64(spanCount)))
			return nil
		}
	} else {
		p.logger.Error(fmt.Sprintf("unexpected descriptor status length from rate limit response: %s ", descriptorStatuses))
	}
	return p.nextConsumer.ConsumeTraces(ctx, traces)
}

func (p *rateLimiterProcessor) getTenantId(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", fmt.Errorf("could not extract headers from context")
	}

	tenantIDHeaders := md.Get(p.tenantIDHeaderName)
	if len(tenantIDHeaders) == 0 {
		return "", fmt.Errorf("missing header: %s", p.tenantIDHeaderName)
	} else if len(tenantIDHeaders) > 1 {
		return "", fmt.Errorf("multiple tenant ID headers were provided, %s: %s", p.tenantIDHeaderName, strings.Join(tenantIDHeaders, ", "))
	}

	return tenantIDHeaders[0], nil
}
