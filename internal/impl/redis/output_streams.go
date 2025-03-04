package redis

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-redis/redis/v7"

	ibatch "github.com/benthosdev/benthos/v4/internal/batch"
	"github.com/benthosdev/benthos/v4/internal/batch/policy"
	"github.com/benthosdev/benthos/v4/internal/bloblang/field"
	"github.com/benthosdev/benthos/v4/internal/bundle"
	"github.com/benthosdev/benthos/v4/internal/component"
	"github.com/benthosdev/benthos/v4/internal/component/output"
	"github.com/benthosdev/benthos/v4/internal/component/output/batcher"
	"github.com/benthosdev/benthos/v4/internal/component/output/processors"
	"github.com/benthosdev/benthos/v4/internal/docs"
	"github.com/benthosdev/benthos/v4/internal/impl/redis/old"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
	"github.com/benthosdev/benthos/v4/internal/metadata"
)

func init() {
	err := bundle.AllOutputs.Add(processors.WrapConstructor(newRedisStreamsOutput), docs.ComponentSpec{
		Name: "redis_streams",
		Summary: `
Pushes messages to a Redis (v5.0+) Stream (which is created if it doesn't
already exist) using the XADD command.`,
		Description: output.Description(true, true, `
It's possible to specify a maximum length of the target stream by setting it to
a value greater than 0, in which case this cap is applied only when Redis is
able to remove a whole macro node, for efficiency.

Redis stream entries are key/value pairs, as such it is necessary to specify the
key to be set to the body of the message. All metadata fields of the message
will also be set as key/value pairs, if there is a key collision between
a metadata item and the body then the body takes precedence.`),
		Config: docs.FieldComponent().WithChildren(old.ConfigDocs()...).WithChildren(
			docs.FieldString("stream", "The stream to add messages to.").IsInterpolated(),
			docs.FieldString("body_key", "A key to set the raw body of the message to."),
			docs.FieldInt("max_length", "When greater than zero enforces a rough cap on the length of the target stream."),
			docs.FieldInt("max_in_flight", "The maximum number of messages to have in flight at a given time. Increase this to improve throughput."),
			docs.FieldObject("metadata", "Specify criteria for which metadata values are included in the message body.").WithChildren(metadata.ExcludeFilterFields()...),
			policy.FieldSpec(),
		).ChildDefaultAndTypesFromStruct(output.NewRedisStreamsConfig()),
		Categories: []string{
			"Services",
		},
	})
	if err != nil {
		panic(err)
	}
}

func newRedisStreamsOutput(conf output.Config, mgr bundle.NewManagement) (output.Streamed, error) {
	w, err := newRedisStreamsWriter(conf.RedisStreams, mgr)
	if err != nil {
		return nil, err
	}
	a, err := output.NewAsyncWriter("redis_streams", conf.RedisStreams.MaxInFlight, w, mgr)
	if err != nil {
		return nil, err
	}
	return batcher.NewFromConfig(conf.RedisStreams.Batching, a, mgr)
}

type redisStreamsWriter struct {
	log log.Modular

	conf       output.RedisStreamsConfig
	stream     *field.Expression
	metaFilter *metadata.ExcludeFilter

	client  redis.UniversalClient
	connMut sync.RWMutex
}

func newRedisStreamsWriter(conf output.RedisStreamsConfig, mgr bundle.NewManagement) (*redisStreamsWriter, error) {
	r := &redisStreamsWriter{
		log:  mgr.Logger(),
		conf: conf,
	}

	var err error
	if r.stream, err = mgr.BloblEnvironment().NewField(conf.Stream); err != nil {
		return nil, fmt.Errorf("failed to parse expression: %v", err)
	}
	if r.metaFilter, err = conf.Metadata.Filter(); err != nil {
		return nil, fmt.Errorf("failed to construct metadata filter: %w", err)
	}

	if _, err = clientFromConfig(conf.Config); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *redisStreamsWriter) Connect(ctx context.Context) error {
	r.connMut.Lock()
	defer r.connMut.Unlock()

	client, err := clientFromConfig(r.conf.Config)
	if err != nil {
		return err
	}
	if _, err = client.Ping().Result(); err != nil {
		return err
	}

	r.log.Infof("Pushing messages to Redis stream: %v\n", r.conf.Stream)

	r.client = client
	return nil
}

func (r *redisStreamsWriter) WriteBatch(ctx context.Context, msg message.Batch) error {
	r.connMut.RLock()
	client := r.client
	r.connMut.RUnlock()

	if client == nil {
		return component.ErrNotConnected
	}

	partToMap := func(p *message.Part) map[string]interface{} {
		values := map[string]interface{}{}
		_ = r.metaFilter.Iter(p, func(k, v string) error {
			values[k] = v
			return nil
		})
		values[r.conf.BodyKey] = p.AsBytes()
		return values
	}

	if msg.Len() == 1 {
		if err := client.XAdd(&redis.XAddArgs{
			ID:           "*",
			Stream:       r.stream.String(0, msg),
			MaxLenApprox: r.conf.MaxLenApprox,
			Values:       partToMap(msg.Get(0)),
		}).Err(); err != nil {
			_ = r.disconnect()
			r.log.Errorf("Error from redis: %v\n", err)
			return component.ErrNotConnected
		}
		return nil
	}

	pipe := client.Pipeline()
	_ = msg.Iter(func(i int, p *message.Part) error {
		_ = pipe.XAdd(&redis.XAddArgs{
			ID:           "*",
			Stream:       r.stream.String(i, msg),
			MaxLenApprox: r.conf.MaxLenApprox,
			Values:       partToMap(p),
		})
		return nil
	})
	cmders, err := pipe.Exec()
	if err != nil {
		_ = r.disconnect()
		r.log.Errorf("Error from redis: %v\n", err)
		return component.ErrNotConnected
	}

	var batchErr *ibatch.Error
	for i, res := range cmders {
		if res.Err() != nil {
			if batchErr == nil {
				batchErr = ibatch.NewError(msg, res.Err())
			}
			batchErr.Failed(i, res.Err())
		}
	}
	if batchErr != nil {
		return batchErr
	}
	return nil
}

func (r *redisStreamsWriter) disconnect() error {
	r.connMut.Lock()
	defer r.connMut.Unlock()
	if r.client != nil {
		err := r.client.Close()
		r.client = nil
		return err
	}
	return nil
}

func (r *redisStreamsWriter) Close(context.Context) error {
	return r.disconnect()
}
