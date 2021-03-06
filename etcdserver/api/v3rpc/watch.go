// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v3rpc

import (
	"io"
	"sync"
	"time"

	"golang.org/x/net/context"

	"github.com/coreos/etcd/etcdserver"
	"github.com/coreos/etcd/etcdserver/api/v3rpc/rpctypes"
	pb "github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/coreos/etcd/mvcc"
	"github.com/coreos/etcd/mvcc/mvccpb"
)

type watchServer struct {
	clusterID int64
	memberID  int64
	raftTimer etcdserver.RaftTimer
	watchable mvcc.Watchable
}

func NewWatchServer(s *etcdserver.EtcdServer) pb.WatchServer {
	return &watchServer{
		clusterID: int64(s.Cluster().ID()),
		memberID:  int64(s.ID()),
		raftTimer: s,
		watchable: s.Watchable(),
	}
}

var (
	// External test can read this with GetProgressReportInterval()
	// and change this to a small value to finish fast with
	// SetProgressReportInterval().
	progressReportInterval   = 10 * time.Minute
	progressReportIntervalMu sync.RWMutex
)

func GetProgressReportInterval() time.Duration {
	progressReportIntervalMu.RLock()
	defer progressReportIntervalMu.RUnlock()
	return progressReportInterval
}

func SetProgressReportInterval(newTimeout time.Duration) {
	progressReportIntervalMu.Lock()
	defer progressReportIntervalMu.Unlock()
	progressReportInterval = newTimeout
}

const (
	// We send ctrl response inside the read loop. We do not want
	// send to block read, but we still want ctrl response we sent to
	// be serialized. Thus we use a buffered chan to solve the problem.
	// A small buffer should be OK for most cases, since we expect the
	// ctrl requests are infrequent.
	ctrlStreamBufLen = 16
)

// serverWatchStream is an etcd server side stream. It receives requests
// from client side gRPC stream. It receives watch events from mvcc.WatchStream,
// and creates responses that forwarded to gRPC stream.
// It also forwards control message like watch created and canceled.
type serverWatchStream struct {
	clusterID int64
	memberID  int64
	raftTimer etcdserver.RaftTimer

	gRPCStream  pb.Watch_WatchServer
	watchStream mvcc.WatchStream
	ctrlStream  chan *pb.WatchResponse

	// progress tracks the watchID that stream might need to send
	// progress to.
	progress map[mvcc.WatchID]bool
	// mu protects progress
	mu sync.Mutex

	// closec indicates the stream is closed.
	closec chan struct{}

	// wg waits for the send loop to complete
	wg sync.WaitGroup
}

func (ws *watchServer) Watch(stream pb.Watch_WatchServer) (err error) {
	sws := serverWatchStream{
		clusterID:   ws.clusterID,
		memberID:    ws.memberID,
		raftTimer:   ws.raftTimer,
		gRPCStream:  stream,
		watchStream: ws.watchable.NewWatchStream(),
		// chan for sending control response like watcher created and canceled.
		ctrlStream: make(chan *pb.WatchResponse, ctrlStreamBufLen),
		progress:   make(map[mvcc.WatchID]bool),
		closec:     make(chan struct{}),
	}

	sws.wg.Add(1)
	go func() {
		sws.sendLoop()
		sws.wg.Done()
	}()

	errc := make(chan error, 1)
	// Ideally recvLoop would also use sws.wg to signal its completion
	// but when stream.Context().Done() is closed, the stream's recv
	// may continue to block since it uses a different context, leading to
	// deadlock when calling sws.close().
	go func() { errc <- sws.recvLoop() }()

	select {
	case err = <-errc:
	case <-stream.Context().Done():
		err = stream.Context().Err()
		// the only server-side cancellation is noleader for now.
		if err == context.Canceled {
			err = rpctypes.ErrGRPCNoLeader
		}
	}
	sws.close()
	return err
}

func (sws *serverWatchStream) recvLoop() error {
	defer close(sws.ctrlStream)
	for {
		req, err := sws.gRPCStream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		switch uv := req.RequestUnion.(type) {
		case *pb.WatchRequest_CreateRequest:
			if uv.CreateRequest == nil {
				break
			}

			creq := uv.CreateRequest
			if len(creq.Key) == 0 {
				// \x00 is the smallest key
				creq.Key = []byte{0}
			}
			if len(creq.RangeEnd) == 1 && creq.RangeEnd[0] == 0 {
				// support  >= key queries
				creq.RangeEnd = []byte{}
			}
			wsrev := sws.watchStream.Rev()
			rev := creq.StartRevision
			if rev == 0 {
				rev = wsrev + 1
			}
			id := sws.watchStream.Watch(creq.Key, creq.RangeEnd, rev)
			if id != -1 && creq.ProgressNotify {
				sws.progress[id] = true
			}
			wr := &pb.WatchResponse{
				Header:   sws.newResponseHeader(wsrev),
				WatchId:  int64(id),
				Created:  true,
				Canceled: id == -1,
			}
			select {
			case sws.ctrlStream <- wr:
			case <-sws.closec:
				return nil
			}
		case *pb.WatchRequest_CancelRequest:
			if uv.CancelRequest != nil {
				id := uv.CancelRequest.WatchId
				err := sws.watchStream.Cancel(mvcc.WatchID(id))
				if err == nil {
					sws.ctrlStream <- &pb.WatchResponse{
						Header:   sws.newResponseHeader(sws.watchStream.Rev()),
						WatchId:  id,
						Canceled: true,
					}
					sws.mu.Lock()
					delete(sws.progress, mvcc.WatchID(id))
					sws.mu.Unlock()
				}
			}
			// TODO: do we need to return error back to client?
		default:
			panic("not implemented")
		}
	}
}

func (sws *serverWatchStream) sendLoop() {
	// watch ids that are currently active
	ids := make(map[mvcc.WatchID]struct{})
	// watch responses pending on a watch id creation message
	pending := make(map[mvcc.WatchID][]*pb.WatchResponse)

	interval := GetProgressReportInterval()
	progressTicker := time.NewTicker(interval)

	defer func() {
		progressTicker.Stop()
		// drain the chan to clean up pending events
		for ws := range sws.watchStream.Chan() {
			mvcc.ReportEventReceived(len(ws.Events))
		}
		for _, wrs := range pending {
			for _, ws := range wrs {
				mvcc.ReportEventReceived(len(ws.Events))
			}
		}
	}()

	for {
		select {
		case wresp, ok := <-sws.watchStream.Chan():
			if !ok {
				return
			}

			// TODO: evs is []mvccpb.Event type
			// either return []*mvccpb.Event from the mvcc package
			// or define protocol buffer with []mvccpb.Event.
			evs := wresp.Events
			events := make([]*mvccpb.Event, len(evs))
			for i := range evs {
				events[i] = &evs[i]
			}

			wr := &pb.WatchResponse{
				Header:          sws.newResponseHeader(wresp.Revision),
				WatchId:         int64(wresp.WatchID),
				Events:          events,
				CompactRevision: wresp.CompactRevision,
			}

			if _, hasId := ids[wresp.WatchID]; !hasId {
				// buffer if id not yet announced
				wrs := append(pending[wresp.WatchID], wr)
				pending[wresp.WatchID] = wrs
				continue
			}

			mvcc.ReportEventReceived(len(evs))
			if err := sws.gRPCStream.Send(wr); err != nil {
				return
			}

			sws.mu.Lock()
			if _, ok := sws.progress[wresp.WatchID]; ok {
				sws.progress[wresp.WatchID] = false
			}
			sws.mu.Unlock()

		case c, ok := <-sws.ctrlStream:
			if !ok {
				return
			}

			if err := sws.gRPCStream.Send(c); err != nil {
				return
			}

			// track id creation
			wid := mvcc.WatchID(c.WatchId)
			if c.Canceled {
				delete(ids, wid)
				continue
			}
			if c.Created {
				// flush buffered events
				ids[wid] = struct{}{}
				for _, v := range pending[wid] {
					mvcc.ReportEventReceived(len(v.Events))
					if err := sws.gRPCStream.Send(v); err != nil {
						return
					}
				}
				delete(pending, wid)
			}
		case <-progressTicker.C:
			for id, ok := range sws.progress {
				if ok {
					sws.watchStream.RequestProgress(id)
				}
				sws.progress[id] = true
			}
		case <-sws.closec:
			return
		}
	}
}

func (sws *serverWatchStream) close() {
	sws.watchStream.Close()
	close(sws.closec)
	sws.wg.Wait()
}

func (sws *serverWatchStream) newResponseHeader(rev int64) *pb.ResponseHeader {
	return &pb.ResponseHeader{
		ClusterId: uint64(sws.clusterID),
		MemberId:  uint64(sws.memberID),
		Revision:  rev,
		RaftTerm:  sws.raftTimer.Term(),
	}
}
