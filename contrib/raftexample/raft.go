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

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"go.etcd.io/etcd/client/pkg/v3/fileutil"
	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"
	"go.etcd.io/etcd/server/v3/etcdserver/api/rafthttp"
	"go.etcd.io/etcd/server/v3/etcdserver/api/snap"
	stats "go.etcd.io/etcd/server/v3/etcdserver/api/v2stats"
	"go.etcd.io/etcd/server/v3/storage/wal"
	"go.etcd.io/etcd/server/v3/storage/wal/walpb"

	"go.uber.org/zap"
)

type commit struct {
	data       []string
	applyDoneC chan<- struct{}
}

// A key-value stream backed by raft
type raftNode struct {
	// 接收来自其它组件传入的需要通过raft达成共识的提议。
	proposeC <-chan string // proposed messages (k,v)
	// 接收来自其它组件的需要通过raft达成共识的集群变更提议。
	confChangeC <-chan raftpb.ConfChange // proposed cluster config changes
	// 用来已通过raft达成共识的已提交的提议通知给其它组件的信道。
	commitC chan<- *commit // entries committed to log (k,v)
	// 用来将错误报告给其它组件的信道。
	errorC chan<- error // errors from raft session

	id    int      // client ID for raft session
	peers []string // raft peer URLs
	// 如果该节点是以加入已有集群的方式启动，那么该值为true；否则是false。
	join        bool   // node is joining an existing cluster
	waldir      string // path to WAL directory
	snapdir     string // path to snapshot directory
	getSnapshot func() ([]byte, error)

	confState raftpb.ConfState
	// 快照中的状态下最后一条日志的索引。
	snapshotIndex uint64
	// 已应用的最后一条日志的索引。
	appliedIndex uint64

	// 下面是 etcd/raft提供的接口与其所需的相关组件

	// 开发者只需要与该接口打交道即可实现基于raft的服务。
	node raft.Node
	// 用来保存raft状态的接口: hardstate, snapshot, log entries
	raftStorage *raft.MemoryStorage
	// wal接口
	wal *wal.WAL
	// 快照管理器
	snapshotter *snap.Snapshotter
	// 通过该信道来通知创建者snapshotter已经加载完成, snapshotter的创建对于新建raftNode来说是一个异步的过程
	snapshotterReady chan *snap.Snapshotter // signals when snapshotter is ready

	// 当wal中的日志超过该值时，触发快照操作并压缩日志。
	snapCount uint64
	// etcd/raft模块通信时使用的接口。同样，这里使用了基于http的默认实现。
	transport *rafthttp.Transport
	// 关闭 proposeC 的信号
	stopc chan struct{} // signals proposal channel closed
	// 通知 http服务器关闭的信号
	httpstopc chan struct{} // signals http server to shutdown
	// http服务器完成关闭的信号
	httpdonec chan struct{} // signals http server shutdown complete

	logger *zap.Logger
}

var defaultSnapshotCount uint64 = 10000

// newRaftNode initiates a raft instance and returns a committed log entry
// channel and error channel. Proposals for log updates are sent over the
// provided the proposal channel. All log entries are replayed over the
// commit channel, followed by a nil message (to indicate the channel is
// current), then new log entries. To shutdown, close proposeC and read errorC.

// 初始化 raftNode 节点，重要的是获取 commitC， errorC， confChangeC 信道
// 然后调用了raftNode的startRaft()方法来启动raft节点
// 将raft模块用来向上通知的commitC， errorC、和快照管理器加载完成信号的信道返回给调用者，去初始化kvstore实例
func newRaftNode(id int, peers []string, join bool, getSnapshot func() ([]byte, error), proposeC <-chan string,
	confChangeC <-chan raftpb.ConfChange) (<-chan *commit, <-chan error, <-chan *snap.Snapshotter) {

	commitC := make(chan *commit)
	errorC := make(chan error)

	rc := &raftNode{
		proposeC:    proposeC,
		confChangeC: confChangeC,
		commitC:     commitC,
		errorC:      errorC,
		id:          id,
		peers:       peers,
		join:        join,
		waldir:      fmt.Sprintf("raftexample-%d", id),
		snapdir:     fmt.Sprintf("raftexample-%d-snap", id),
		getSnapshot: getSnapshot,
		snapCount:   defaultSnapshotCount,
		stopc:       make(chan struct{}),
		httpstopc:   make(chan struct{}),
		httpdonec:   make(chan struct{}),

		logger: zap.NewExample(),

		snapshotterReady: make(chan *snap.Snapshotter, 1),
		// rest of structure populated after WAL replay
	}
	// 异步初始化raftNode
	go rc.startRaft()
	return commitC, errorC, rc.snapshotterReady
}

// snapshot存到snapshotter的dir字段指示的目录下
// snapshot元数据存到wal
func (rc *raftNode) saveSnap(snap raftpb.Snapshot) error {
	walSnap := walpb.Snapshot{
		Index:     snap.Metadata.Index,
		Term:      snap.Metadata.Term,
		ConfState: &snap.Metadata.ConfState,
	}
	// save the snapshot file before writing the snapshot to the wal.
	// This makes it possible for the snapshot file to become orphaned, but prevents
	// a WAL snapshot entry from having no corresponding snapshot file.
	if err := rc.snapshotter.SaveSnap(snap); err != nil {
		return err
	}
	if err := rc.wal.SaveSnapshot(walSnap); err != nil {
		return err
	}
	return rc.wal.ReleaseLockTo(snap.Metadata.Index)
}

func (rc *raftNode) entriesToApply(ents []raftpb.Entry) (nents []raftpb.Entry) {
	if len(ents) == 0 {
		return ents
	}
	firstIdx := ents[0].Index
	if firstIdx > rc.appliedIndex+1 {
		log.Fatalf("first index of committed entry[%d] should <= progress.appliedIndex[%d]+1", firstIdx, rc.appliedIndex)
	}
	if rc.appliedIndex-firstIdx+1 < uint64(len(ents)) {
		nents = ents[rc.appliedIndex-firstIdx+1:]
	}
	return nents
}

// publishEntries writes committed log entries to commit channel and returns
// whether all entries could be published.
func (rc *raftNode) publishEntries(ents []raftpb.Entry) (<-chan struct{}, bool) {
	if len(ents) == 0 {
		return nil, true
	}

	data := make([]string, 0, len(ents))
	// 遍历所有log entries，统计所以普通条目， 传给kvstore。关于集群配置变更的条目，直接应用
	for i := range ents {
		switch ents[i].Type {
		// 反序列化普通日志条目
		case raftpb.EntryNormal:
			if len(ents[i].Data) == 0 {
				// ignore empty messages
				break
			}
			s := string(ents[i].Data)
			data = append(data, s)
		// 对于用于变更集群配置的日志，则根据变更的内容（如增加或删除集群中的某个节点），修改通信模块中的相关记录
		case raftpb.EntryConfChange:
			var cc raftpb.ConfChange
			cc.Unmarshal(ents[i].Data)
			rc.confState = *rc.node.ApplyConfChange(cc)
			switch cc.Type {
			case raftpb.ConfChangeAddNode:
				if len(cc.Context) > 0 {
					rc.transport.AddPeer(types.ID(cc.NodeID), []string{string(cc.Context)})
				}
			case raftpb.ConfChangeRemoveNode:
				if cc.NodeID == uint64(rc.id) {
					log.Println("I've been removed from the cluster! Shutting down.")
					return nil, false
				}
				rc.transport.RemovePeer(types.ID(cc.NodeID))
			}
		}
	}

	var applyDoneC chan struct{}

	// 在该goroutinue中同步去传commit的数据
	if len(data) > 0 {
		applyDoneC = make(chan struct{}, 1)
		select {
		case rc.commitC <- &commit{data, applyDoneC}:
		case <-rc.stopc:
			return nil, false
		}
	}

	// after commit, update appliedIndex
	rc.appliedIndex = ents[len(ents)-1].Index

	return applyDoneC, true
}

func (rc *raftNode) loadSnapshot() *raftpb.Snapshot {
	if wal.Exist(rc.waldir) {
		walSnaps, err := wal.ValidSnapshotEntries(rc.logger, rc.waldir)
		if err != nil {
			log.Fatalf("raftexample: error listing snapshots (%v)", err)
		}
		snapshot, err := rc.snapshotter.LoadNewestAvailable(walSnaps)
		if err != nil && err != snap.ErrNoSnapshot {
			log.Fatalf("raftexample: error loading snapshot (%v)", err)
		}
		return snapshot
	}
	return &raftpb.Snapshot{}
}

// openWAL returns a WAL ready for reading.
func (rc *raftNode) openWAL(snapshot *raftpb.Snapshot) *wal.WAL {
	if !wal.Exist(rc.waldir) {
		if err := os.Mkdir(rc.waldir, 0750); err != nil {
			log.Fatalf("raftexample: cannot create dir for wal (%v)", err)
		}

		w, err := wal.Create(zap.NewExample(), rc.waldir, nil)
		if err != nil {
			log.Fatalf("raftexample: create wal error (%v)", err)
		}
		w.Close()
	}

	walsnap := walpb.Snapshot{}
	if snapshot != nil {
		walsnap.Index, walsnap.Term = snapshot.Metadata.Index, snapshot.Metadata.Term
	}
	log.Printf("loading WAL at term %d and index %d", walsnap.Term, walsnap.Index)
	w, err := wal.Open(zap.NewExample(), rc.waldir, walsnap)
	if err != nil {
		log.Fatalf("raftexample: error loading wal (%v)", err)
	}

	return w
}

// replayWAL replays WAL entries into the raft instance.
// 先检测有无wal，snapshot，有的话打开，没有就创建，然后重建memorystorage
func (rc *raftNode) replayWAL() *wal.WAL {
	log.Printf("replaying WAL of member %d", rc.id)
	// 加载快照
	snapshot := rc.loadSnapshot()
	// 根据快照的term，index，打开相应的wal。快照不存在，则为其创建一个初始状态的wal（节点第一次启动既没有快照文件又没有预写日志文件）
	w := rc.openWAL(snapshot)
	_, st, ents, err := w.ReadAll()
	if err != nil {
		log.Fatalf("raftexample: failed to read WAL (%v)", err)
	}

	// 将snapshot应用到memorystorage，wal中的HardState应用到存储中，以及未replay的log entries添加到存储中
	rc.raftStorage = raft.NewMemoryStorage()
	if snapshot != nil {
		rc.raftStorage.ApplySnapshot(*snapshot)
	}
	rc.raftStorage.SetHardState(st)

	// append to storage so raft starts at the right place in log
	// 日志的重放逻辑没有在replayWAL中实现
	rc.raftStorage.Append(ents)

	return w
}

func (rc *raftNode) writeError(err error) {
	rc.stopHTTP()
	close(rc.commitC)
	rc.errorC <- err
	close(rc.errorC)
	rc.node.Stop()
}

// 1.检查快照目录是否存在，如果不存在为其创建目录。然后创建基于该目录的快照管理器。
//	 向snapshotterReady信道写入该快照管理器，通知其快照管理器已经创建完成
// 2.检查是否有旧的预写日志存在，并重放旧的预写日志到raftstorage
// 3.设置了etcd/raft模块所需的配置，并从该配置上启动或重启节点(根据是否存在oldwal重启新的还是旧的raft节点)
// 4.配置并开启了通信模块，开始与集群中的其它raft节点通信。
// 5.在一切接续后，程序启动了两个goroutine
func (rc *raftNode) startRaft() {
	if !fileutil.Exist(rc.snapdir) {
		if err := os.Mkdir(rc.snapdir, 0750); err != nil {
			log.Fatalf("raftexample: cannot create dir for snapshot (%v)", err)
		}
	}
	rc.snapshotter = snap.New(zap.NewExample(), rc.snapdir)

	oldwal := wal.Exist(rc.waldir)
	rc.wal = rc.replayWAL()

	// signal replay has finished
	// Newkvstore函数在收到snapshotter创建完毕的消息后才能继续运行，否则阻塞。
	rc.snapshotterReady <- rc.snapshotter

	rpeers := make([]raft.Peer, len(rc.peers))
	for i := range rpeers {
		rpeers[i] = raft.Peer{ID: uint64(i + 1)}
	}
	c := &raft.Config{
		ID:                        uint64(rc.id),
		ElectionTick:              10,
		HeartbeatTick:             1,
		Storage:                   rc.raftStorage,
		MaxSizePerMsg:             1024 * 1024,
		MaxInflightMsgs:           256,
		MaxUncommittedEntriesSize: 1 << 30,
	}

	if oldwal || rc.join {
		rc.node = raft.RestartNode(c)
	} else {
		rc.node = raft.StartNode(c, rpeers)
	}

	rc.transport = &rafthttp.Transport{
		Logger:      rc.logger,
		ID:          types.ID(rc.id),
		ClusterID:   0x1000,
		Raft:        rc,
		ServerStats: stats.NewServerStats("", ""),
		LeaderStats: stats.NewLeaderStats(zap.NewExample(), strconv.Itoa(rc.id)),
		ErrorC:      make(chan error),
	}

	rc.transport.Start()
	for i := range rc.peers {
		if i+1 != rc.id {
			rc.transport.AddPeer(types.ID(i+1), []string{rc.peers[i]})
		}
	}
	// 用来监听来自其它raft节点的消息，消息的处理主要在Transport接口的实现中编写
	go rc.serveRaft()
	// 用来处理raftNode中各种信道
	go rc.serveChannels()
}

// stop closes http, closes all channels, and stops raft.
func (rc *raftNode) stop() {
	rc.stopHTTP()
	close(rc.commitC)
	close(rc.errorC)
	rc.node.Stop()
}

func (rc *raftNode) stopHTTP() {
	rc.transport.Stop()
	close(rc.httpstopc)
	<-rc.httpdonec
}

func (rc *raftNode) publishSnapshot(snapshotToSave raftpb.Snapshot) {
	if raft.IsEmptySnap(snapshotToSave) {
		return
	}

	log.Printf("publishing snapshot at index %d", rc.snapshotIndex)
	defer log.Printf("finished publishing snapshot at index %d", rc.snapshotIndex)

	if snapshotToSave.Metadata.Index <= rc.appliedIndex {
		log.Fatalf("snapshot index [%d] should > progress.appliedIndex [%d]", snapshotToSave.Metadata.Index, rc.appliedIndex)
	}
	rc.commitC <- nil // trigger kvstore to load snapshot

	rc.confState = snapshotToSave.Metadata.ConfState
	rc.snapshotIndex = snapshotToSave.Metadata.Index
	rc.appliedIndex = snapshotToSave.Metadata.Index
}

var snapshotCatchUpEntriesN uint64 = 10000

func (rc *raftNode) maybeTriggerSnapshot(applyDoneC <-chan struct{}) {
	if rc.appliedIndex-rc.snapshotIndex <= rc.snapCount {
		return
	}

	// wait until all committed entries are applied (or server is closed)
	if applyDoneC != nil {
		select {
		case <-applyDoneC:
		case <-rc.stopc:
			return
		}
	}

	log.Printf("start snapshot [applied index: %d | last snapshot index: %d]", rc.appliedIndex, rc.snapshotIndex)
	data, err := rc.getSnapshot()
	if err != nil {
		log.Panic(err)
	}
	snap, err := rc.raftStorage.CreateSnapshot(rc.appliedIndex, &rc.confState, data)
	if err != nil {
		panic(err)
	}
	if err := rc.saveSnap(snap); err != nil {
		panic(err)
	}

	compactIndex := uint64(1)
	if rc.appliedIndex > snapshotCatchUpEntriesN {
		compactIndex = rc.appliedIndex - snapshotCatchUpEntriesN
	}
	if err := rc.raftStorage.Compact(compactIndex); err != nil {
		panic(err)
	}

	log.Printf("compacted log at index %d", compactIndex)
	rc.snapshotIndex = rc.appliedIndex
}

// raft服务器用来处理各种信道的输入输出的方法，也是与etcd/raft模块中Node接口的实现交互的方法。
// 循环处理raft有关的逻辑，如处理定时器信号驱动Node、处理Node传入的Ready结构体、处理通信模块报告的错误或停止信号灯等
// 还启动了一个goroutine，该goroutine中循环处理来自proposeC和confChangeC两个信道的消息
func (rc *raftNode) serveChannels() {
	// 根据snapshot设置raftNode相关字段
	snap, err := rc.raftStorage.Snapshot()
	if err != nil {
		panic(err)
	}
	rc.confState = snap.Metadata.ConfState
	rc.snapshotIndex = snap.Metadata.Index
	rc.appliedIndex = snap.Metadata.Index

	defer rc.wal.Close()
	// 产生一个循环定时器驱动raft状态机
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	// send proposals over raft
	// proposeC接收来自kvstore的提议（http server --> kvstore servere --> raftNode --> raft状态机）
	// confChangeC接收来自上层的提议 (http server --> raftNode --> raft状态机)
	go func() {
		confChangeCount := uint64(0)

		for rc.proposeC != nil && rc.confChangeC != nil {
			select {
			case prop, ok := <-rc.proposeC:
				if !ok {
					rc.proposeC = nil
				} else {
					// blocks until accepted by raft state machine
					rc.node.Propose(context.TODO(), []byte(prop))
				}

			case cc, ok := <-rc.confChangeC:
				if !ok {
					rc.confChangeC = nil
				} else {
					confChangeCount++
					cc.ID = confChangeCount
					rc.node.ProposeConfChange(context.TODO(), cc)
				}
			}
		}
		// client closed channel; shutdown raft if not already
		// 如果两个信道之一被关闭了，关闭raft服务器
		close(rc.stopc)
	}()

	// event loop on raft state machine updates
	for {
		select {
		// tick 驱动raft状态机
		case <-ticker.C:
			rc.node.Tick()

		// store raft entries to wal, then publish over commit channel
		// 每当Node准备好一批数据后，会将数据通过该信道发布。
		// 开发者需要对该信道收到的Ready结构体中的各字段进行处理。
		// 在处理完成一批数据后，开发者还需要调用Node.Advance()告知Node这批数据已处理完成，可以继续传入下一批数据。
		// 处理流程：
		// 1.将HardState和Entries写入wal，将其保存在稳定存储上。
		// 2.如果有快照，现将快照保存到wal，然后应用到memorystorage，最后通过向commitC写入nil值通知kvstore加载快照。
		// 3.将Entries追加到MemoryStorage中（第1步仅写入到了预写日志中）。
		// 4.通过通信模块将Messages中的消息分发给其它raft节点
		// 5.通过publishEntries方法发布新增的日志条目
		// 6.通过maybeTriggerSnapshot方法检查MemoryStorage中日志条目长度，如果超过设定的最大长度，则触发快照机制并压缩日志。
		// 7.调用Node.Advance()告知Node这批数据已处理完成，可以继续传入下一批数据
		case rd := <-rc.node.Ready():
			rc.wal.Save(rd.HardState, rd.Entries)
			if !raft.IsEmptySnap(rd.Snapshot) {
				rc.saveSnap(rd.Snapshot)
				rc.raftStorage.ApplySnapshot(rd.Snapshot)
				rc.publishSnapshot(rd.Snapshot)
			}
			rc.raftStorage.Append(rd.Entries)
			rc.transport.Send(rc.processMessages(rd.Messages))
			applyDoneC, ok := rc.publishEntries(rc.entriesToApply(rd.CommittedEntries)) // applyDoneC是传给kvstore的，当kvstore将所有条目apply到状态机上，关闭此信道
			if !ok {
				rc.stop()
				return
			}
			rc.maybeTriggerSnapshot(applyDoneC)
			rc.node.Advance()

		// 通信模块报错信道，收到来自该信道的错误后raftNode会继续上报该错误，并关闭raftNode
		case err := <-rc.transport.ErrorC:
			rc.writeError(err)
			return
		// 用来表示停止信号的信道，当该信道被关闭时，关闭raftNode
		case <-rc.stopc:
			rc.stop()
			return
		}
	}
}

// When there is a `raftpb.EntryConfChange` after creating the snapshot,
// then the confState included in the snapshot is out of date. so We need
// to update the confState before sending a snapshot to a follower.
func (rc *raftNode) processMessages(ms []raftpb.Message) []raftpb.Message {
	for i := 0; i < len(ms); i++ {
		if ms[i].Type == raftpb.MsgSnap {
			ms[i].Snapshot.Metadata.ConfState = rc.confState
		}
	}
	return ms
}

func (rc *raftNode) serveRaft() {
	url, err := url.Parse(rc.peers[rc.id-1])
	if err != nil {
		log.Fatalf("raftexample: Failed parsing URL (%v)", err)
	}

	ln, err := newStoppableListener(url.Host, rc.httpstopc)
	if err != nil {
		log.Fatalf("raftexample: Failed to listen rafthttp (%v)", err)
	}

	err = (&http.Server{Handler: rc.transport.Handler()}).Serve(ln)
	select {
	case <-rc.httpstopc:
	default:
		log.Fatalf("raftexample: Failed to serve rafthttp (%v)", err)
	}
	close(rc.httpdonec)
}

func (rc *raftNode) Process(ctx context.Context, m raftpb.Message) error {
	return rc.node.Step(ctx, m)
}
func (rc *raftNode) IsIDRemoved(id uint64) bool  { return false }
func (rc *raftNode) ReportUnreachable(id uint64) { rc.node.ReportUnreachable(id) }
func (rc *raftNode) ReportSnapshot(id uint64, status raft.SnapshotStatus) {
	rc.node.ReportSnapshot(id, status)
}
