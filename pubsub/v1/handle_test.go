/*
Copyright 2022 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pubsub

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dapr-sandbox/components-go-sdk/internal"
	contribPubSub "github.com/dapr/components-contrib/pubsub"
	proto "github.com/dapr/dapr/pkg/proto/components/v1"
	"github.com/stretchr/testify/assert"
)

type recvFakeResp struct {
	msg *proto.PullMessagesRequest
	err error
}
type fakeTsStream struct {
	sendCalled   atomic.Int64
	onSendCalled func(*proto.PullMessagesResponse)
	sendErr      error
	recvCalled   atomic.Int64
	recvChan     chan *recvFakeResp
}

func (f *fakeTsStream) Send(msg *proto.PullMessagesResponse) error {
	f.sendCalled.Add(1)
	if f.onSendCalled != nil {
		f.onSendCalled(msg)
	}
	return f.sendErr
}
func (f *fakeTsStream) Recv() (*proto.PullMessagesRequest, error) {
	f.recvCalled.Add(1)
	var (
		resp *proto.PullMessagesRequest
		err  error
	)
	if f.recvChan != nil {
		fakeResp := <-f.recvChan
		resp = fakeResp.msg
		err = fakeResp.err
	}
	return resp, err
}

func TestAckLoop(t *testing.T) {
	t.Run("ack loop should return nil when stream returns EOF", func(t *testing.T) {
		recvChan := make(chan *recvFakeResp, 1)
		recvChan <- &recvFakeResp{
			err: io.EOF,
		}
		close(recvChan)
		stream := &fakeTsStream{recvChan: recvChan}
		assert.Nil(t, ackLoop(context.Background(), stream, nil))
		assert.Equal(t, int64(1), stream.recvCalled.Load())
	})
	t.Run("ack should be called with nil when no error is returned", func(t *testing.T) {
		ack := internal.NewAckManager[error]()
		msgID, c, _ := ack.Get()
		recvChan := make(chan *recvFakeResp, 2)
		recvChan <- &recvFakeResp{
			msg: &proto.PullMessagesRequest{
				AckMessageId: msgID,
			},
		}
		recvChan <- &recvFakeResp{
			err: io.EOF,
		}
		close(recvChan)
		stream := &fakeTsStream{recvChan: recvChan}
		assert.NotEmpty(t, ack.Pending())
		assert.Nil(t, ackLoop(context.Background(), stream, ack))
		assert.Equal(t, int64(2), stream.recvCalled.Load())
		err, notClosed := <-c
		assert.True(t, notClosed)
		assert.Nil(t, err)
	})
	t.Run("ack should be called with err when error is returned", func(t *testing.T) {
		const fakeMsg = "fake-err"
		ack := internal.NewAckManager[error]()
		msgID, c, _ := ack.Get()
		recvChan := make(chan *recvFakeResp, 2)
		recvChan <- &recvFakeResp{
			msg: &proto.PullMessagesRequest{
				AckMessageId: msgID,
				AckError: &proto.AckMessageError{
					Message: fakeMsg,
				},
			},
		}
		recvChan <- &recvFakeResp{
			err: io.EOF,
		}
		close(recvChan)
		stream := &fakeTsStream{recvChan: recvChan}
		assert.NotEmpty(t, ack.Pending())
		assert.Nil(t, ackLoop(context.Background(), stream, ack))
		assert.Equal(t, int64(2), stream.recvCalled.Load())
		err, notClosed := <-c
		assert.True(t, notClosed)
		assert.Equal(t, err.Error(), fakeMsg)
	})
}

func TestHandler(t *testing.T) {
	t.Run("when send returns an error so handler should return an error and cleanup pending acks", func(t *testing.T) {
		sendErr := errors.New("fake-err")
		stream := &fakeTsStream{sendErr: sendErr}
		acks := internal.NewAckManager[error]()
		handlerf := handler(stream, acks)

		assert.NotNil(t, handlerf(context.TODO(), &contribPubSub.NewMessage{}))
		assert.Empty(t, acks.Pending())
		assert.Equal(t, int64(1), stream.sendCalled.Load())
	})
	t.Run("handle should return Acktimeout when context is done", func(t *testing.T) {
		stream := &fakeTsStream{}
		acks := internal.NewAckManager[error]()
		handlerf := handler(stream, acks)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		assert.Equal(t, ErrAckTimeout, handlerf(ctx, &contribPubSub.NewMessage{}))
		assert.Empty(t, acks.Pending())
		assert.Equal(t, int64(1), stream.sendCalled.Load())
	})
	t.Run("handle should return pending ack error", func(t *testing.T) {
		fakeErr := errors.New("fake-err")
		var sendCalledWg sync.WaitGroup
		sendCalledWg.Add(1)
		stream := &fakeTsStream{
			onSendCalled: func(*proto.PullMessagesResponse) {
				sendCalledWg.Done()
			},
		}
		acks := internal.NewAckManager[error]()
		handlerf := handler(stream, acks)
		go func() {
			sendCalledWg.Wait()
			for _, pendingAck := range acks.Pending() {
				pendingAck <- fakeErr
			}
		}()

		assert.Equal(t, fakeErr, handlerf(context.Background(), &contribPubSub.NewMessage{}))
		assert.Empty(t, acks.Pending())
		assert.Equal(t, int64(1), stream.sendCalled.Load())
	})
}
