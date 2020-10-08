//
// (C) Copyright 2020 Intel Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// GOVERNMENT LICENSE RIGHTS-OPEN SOURCE SOFTWARE
// The Government's rights to use, modify, reproduce, release, perform, display,
// or disclose this software are subject to the terms of the Apache License as
// provided in Contract No. 8F-30005.
// Any reproduction of computer software, computer software documentation, or
// portions thereof marked with this legend must also reproduce the markings.
//

package system

import (
	"context"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/raft"
	"github.com/pkg/errors"

	"github.com/daos-stack/daos/src/control/build"
	"github.com/daos-stack/daos/src/control/common"
	"github.com/daos-stack/daos/src/control/lib/atm"
	"github.com/daos-stack/daos/src/control/logging"
)

const (
	CurrentSchemaVersion = 0
)

type (
	onLeadershipGainedFn func(context.Context) error
	onLeadershipLostFn   func() error

	raftService interface {
		Apply([]byte, time.Duration) raft.ApplyFuture
		AddVoter(raft.ServerID, raft.ServerAddress, uint64, time.Duration) raft.IndexFuture
		BootstrapCluster(raft.Configuration) raft.Future
		Leader() raft.ServerAddress
		LeaderCh() <-chan bool
		LeadershipTransfer() raft.Future
		Shutdown() raft.Future
		State() raft.RaftState
	}

	// dbData is the raft-replicated system database. It
	// should never be updated directly; updates must be
	// applied in order to ensure that they are sent to
	// all participating replicas.
	dbData struct {
		sync.RWMutex
		log logging.Logger

		NextRank      Rank
		MapVersion    uint32
		Members       *MemberDatabase
		Pools         *PoolDatabase
		SchemaVersion uint
	}

	// syncTCPAddr protects a TCP address with a mutex to allow
	// for atomic reads and writes.
	syncTCPAddr struct {
		sync.RWMutex
		Addr *net.TCPAddr
	}

	// Database provides high-level access methods for the
	// system data as well as structure for managing the raft
	// service that replicates the system data.
	Database struct {
		sync.Mutex
		log                logging.Logger
		cfg                *DatabaseConfig
		replicaAddr        *syncTCPAddr
		isBootstrap        atm.Bool
		raft               raftService
		raftTransport      raft.Transport
		onLeadershipGained []onLeadershipGainedFn
		onLeadershipLost   []onLeadershipLostFn

		data *dbData
	}

	// DatabaseConfig defines the configuration for the system database.
	DatabaseConfig struct {
		LocalAddr  *net.TCPAddr
		Replicas   []string
		RaftDir    string
		SystemName string
	}

	// GroupMap represents a version of the system membership map.
	GroupMap struct {
		Version  uint32
		RankURIs map[Rank]string
	}
)

func (sta *syncTCPAddr) String() string {
	if sta == nil || sta.Addr == nil {
		return "(nil)"
	}
	return sta.Addr.String()
}

func (sta *syncTCPAddr) set(addr *net.TCPAddr) {
	sta.Lock()
	defer sta.Unlock()
	sta.Addr = addr
}

func (sta *syncTCPAddr) get() *net.TCPAddr {
	sta.RLock()
	defer sta.RUnlock()
	return sta.Addr
}

// NewDatabase returns a configured and initialized Database instance.
func NewDatabase(log logging.Logger, cfg *DatabaseConfig) (*Database, error) {
	if cfg == nil {
		cfg = &DatabaseConfig{}
	}

	if cfg.SystemName == "" {
		cfg.SystemName = build.DefaultSystemName
	}

	db := &Database{
		log:         log,
		cfg:         cfg,
		replicaAddr: &syncTCPAddr{},

		data: &dbData{
			log: log,

			Members: &MemberDatabase{
				Ranks: make(MemberRankMap),
				Uuids: make(MemberUuidMap),
				Addrs: make(MemberAddrMap),
			},
			Pools: &PoolDatabase{
				Ranks: make(PoolRankMap),
				Uuids: make(PoolUuidMap),
			},
			SchemaVersion: CurrentSchemaVersion,
		},
	}

	if db.cfg.LocalAddr != nil {
		replicaAddr, isBootstrap, err := db.checkReplica(db.cfg.LocalAddr)
		if err != nil {
			return nil, err
		}
		db.setReplica(replicaAddr)
		if isBootstrap {
			db.isBootstrap.SetTrue()
		}
	}

	return db, nil
}

// resolveReplicas converts the string-based representations of replica
// addresses to a slice of resolved addresses, or returns an error.
func (db *Database) resolveReplicas() (reps []*net.TCPAddr, err error) {
	for _, rs := range db.cfg.Replicas {
		rAddr, err := net.ResolveTCPAddr("tcp", rs)
		if err != nil {
			return nil, err
		}
		reps = append(reps, rAddr)
	}
	return
}

// checkReplica compares the supplied control address to the list of
// configured replica addresses. If it matches, then the resolved
// replica address is returned. The first replica is used to bootstrap
// the raft service.
func (db *Database) checkReplica(ctrlAddr *net.TCPAddr) (repAddr *net.TCPAddr, isBootStrap bool, err error) {
	var repAddrs []*net.TCPAddr
	repAddrs, err = db.resolveReplicas()
	if err != nil {
		return
	}

	for idx, candidate := range repAddrs {
		if candidate.Port != ctrlAddr.Port {
			continue
		}
		if common.IsLocalAddr(candidate) {
			repAddr = candidate
			if idx == 0 {
				isBootStrap = true
			}
			return
		}
	}

	return
}

// SystemName returns the system name set in the configuration.
func (db *Database) SystemName() string {
	return db.cfg.SystemName
}

// LeaderQuery returns the system leader, if known.
func (db *Database) LeaderQuery() (leader string, replicas []string, err error) {
	if !db.IsReplica() {
		return "", nil, &ErrNotReplica{db.cfg.Replicas}
	}
	return string(db.raft.Leader()), db.cfg.Replicas, nil
}

// ReplicaAddr returns the system's replica address if
// the system is configured as a MS replica.
func (db *Database) ReplicaAddr() (*net.TCPAddr, error) {
	if !db.IsReplica() {
		return nil, &ErrNotReplica{db.cfg.Replicas}
	}
	return db.getReplica(), nil
}

func (db *Database) getReplica() *net.TCPAddr {
	return db.replicaAddr.get()
}

func (db *Database) setReplica(addr *net.TCPAddr) {
	db.replicaAddr.set(addr)
	db.log.Debugf("set db replica addr: %s", addr)
}

func (db *Database) IsReplica() bool {
	return db != nil && db.getReplica() != nil
}

func (db *Database) IsBootstrap() bool {
	return db.IsReplica() && db.isBootstrap.IsTrue()
}

func (db *Database) checkLeader() error {
	if db.raft == nil || !db.IsReplica() {
		return &ErrNotReplica{db.cfg.Replicas}
	}
	if db.raft.State() != raft.Leader {
		return &ErrNotLeader{
			LeaderHint: db.leaderHint(),
			Replicas:   db.cfg.Replicas,
		}
	}
	return nil
}

func (db *Database) leaderHint() string {
	if db.raft != nil {
		return string(db.raft.Leader())
	}
	return ""
}

// IsLeader returns a boolean indicating whether or not this
// system thinks that is a) a replica and b) the current leader.
func (db *Database) IsLeader() bool {
	return db.checkLeader() == nil
}

// OnLeadershipGained registers callbacks to be run when this instance
// gains the leadership role.
func (db *Database) OnLeadershipGained(fns ...onLeadershipGainedFn) {
	db.onLeadershipGained = append(db.onLeadershipGained, fns...)
}

// OnLeadershipLost registers callbacks to be run when this instance
// loses the leadership role.
func (db *Database) OnLeadershipLost(fns ...onLeadershipLostFn) {
	db.onLeadershipLost = append(db.onLeadershipLost, fns...)
}

// Start checks to see if the system is configured as a MS replica. If
// not, it returns early without an error. If it is, the persistent storage
// is initialized if necessary, and the replica is started to begin the
// process of choosing a MS leader.
func (db *Database) Start(ctx context.Context) error {
	if !db.IsReplica() {
		return nil
	}

	db.log.Debugf("system db start: isReplica: %t, isBootstrap: %t", db.IsReplica(), db.IsBootstrap())

	var newDB bool

	if _, err := os.Stat(db.cfg.RaftDir); err != nil {
		if !os.IsNotExist(err) {
			return errors.Wrapf(err, "can't Stat() %s", db.cfg.RaftDir)
		}
		newDB = true
		if err := os.Mkdir(db.cfg.RaftDir, 0700); err != nil {
			return errors.Wrapf(err, "failed to Mkdir() %s", db.cfg.RaftDir)
		}
	}

	if err := db.configureRaft(); err != nil {
		return errors.Wrap(err, "unable to configure raft service")
	}

	if err := db.startRaft(newDB); err != nil {
		return errors.Wrap(err, "unable to start raft service")
	}

	// Kick off a goroutine to monitor the leadership state channel.
	go db.monitorLeadershipState(ctx)

	return nil
}

func (db *Database) monitorLeadershipState(parent context.Context) {
	var cancelGainedCtx context.CancelFunc
	for {
		select {
		case <-parent.Done():
			if cancelGainedCtx != nil {
				cancelGainedCtx()
			}
			_ = db.raft.Shutdown().Error()
			return
		case isLeader := <-db.raft.LeaderCh():
			if !isLeader {
				db.log.Debugf("node %s lost MS leader state", db.replicaAddr)
				if cancelGainedCtx != nil {
					cancelGainedCtx()
				}

				for _, fn := range db.onLeadershipLost {
					if err := fn(); err != nil {
						db.log.Errorf("failure in onLeadershipLost callback: %s", err)
					}
				}

				return
			}

			db.log.Debugf("node %s gained MS leader state", db.replicaAddr)
			var gainedCtx context.Context
			gainedCtx, cancelGainedCtx = context.WithCancel(parent)
			for _, fn := range db.onLeadershipGained {
				if err := fn(gainedCtx); err != nil {
					db.log.Errorf("failure in onLeadershipGained callback: %s", err)
					cancelGainedCtx()
					_ = db.ResignLeadership(err)
					break
				}
			}

		}
	}
}

// ResignLeadership causes this instance to give up its raft
// leadership state.
func (db *Database) ResignLeadership(cause error) error {
	// NB: This is effectively a no-op at the moment because
	// there is no one to take over! If no replicas are detected,
	// the raft service continues to run. Leaving this enabled
	// so that we can see it in logs in case there are unexpected
	// resignation events.
	if cause == nil {
		cause = errors.New("unknown error")
	}
	db.log.Errorf("resigning leadership (%s)", cause)
	if err := db.raft.LeadershipTransfer().Error(); err != nil {
		return errors.Wrap(err, cause.Error())
	}
	return cause
}

func newGroupMap(version uint32) *GroupMap {
	return &GroupMap{
		Version:  version,
		RankURIs: make(map[Rank]string),
	}
}

// GroupMap returns the latest system group map.
func (db *Database) GroupMap() (*GroupMap, error) {
	if err := db.checkLeader(); err != nil {
		return nil, err
	}
	db.data.RLock()
	defer db.data.RUnlock()

	gm := newGroupMap(db.data.MapVersion)
	for _, srv := range db.data.Members.Ranks {
		gm.RankURIs[srv.Rank] = srv.FabricURI
	}
	return gm, nil
}

// ReplicaRanks returns the set of ranks associated with MS replicas.
func (db *Database) ReplicaRanks() (*GroupMap, error) {
	if err := db.checkLeader(); err != nil {
		return nil, err
	}
	db.data.RLock()
	defer db.data.RUnlock()

	// TODO: Should this only return one rank per replica, or
	// should we return all ready ranks per replica, for resiliency?
	gm := newGroupMap(db.data.MapVersion)
	for _, srv := range db.data.Members.Ranks {
		repAddr, _, err := db.checkReplica(srv.Addr)
		if err != nil || repAddr == nil ||
			!(srv.state == MemberStateJoined || srv.state == MemberStateReady) {

			continue
		}
		gm.RankURIs[srv.Rank] = srv.FabricURI
	}
	return gm, nil
}

// AllMembers returns a copy of the system membership.
func (db *Database) AllMembers() ([]*Member, error) {
	if err := db.checkLeader(); err != nil {
		return nil, err
	}
	db.data.RLock()
	defer db.data.RUnlock()

	// NB: This is expensive! We make a copy of the
	// membership to ensure that it can't be changed
	// elsewhere.
	dbCopy := make([]*Member, len(db.data.Members.Uuids))
	copyIdx := 0
	for _, dbRec := range db.data.Members.Uuids {
		dbCopy[copyIdx] = new(Member)
		*dbCopy[copyIdx] = *dbRec
		copyIdx++
	}
	return dbCopy, nil
}

// MemberRanks returns a slice of all the ranks in the membership.
func (db *Database) MemberRanks() ([]Rank, error) {
	if err := db.checkLeader(); err != nil {
		return nil, err
	}
	db.data.RLock()
	defer db.data.RUnlock()

	ranks := make([]Rank, 0, len(db.data.Members.Ranks))
	for rank := range db.data.Members.Ranks {
		ranks = append(ranks, rank)
	}

	sort.Slice(ranks, func(i, j int) bool { return ranks[i] < ranks[j] })

	return ranks, nil
}

// MemberCount returns the number of members in the system.
func (db *Database) MemberCount() (int, error) {
	if err := db.checkLeader(); err != nil {
		return -1, err
	}
	db.data.RLock()
	defer db.data.RUnlock()

	return len(db.data.Members.Ranks), nil
}

// CurMapVersion returns the current system map version.
func (db *Database) CurMapVersion() (uint32, error) {
	if err := db.checkLeader(); err != nil {
		return 0, err
	}
	db.data.RLock()
	defer db.data.RUnlock()

	return db.data.MapVersion, nil
}

// RemoveMember removes a member from the system.
func (db *Database) RemoveMember(m *Member) error {
	if err := db.checkLeader(); err != nil {
		return err
	}
	db.Lock()
	defer db.Unlock()

	_, err := db.FindMemberByUUID(m.UUID)
	if err != nil {
		return err
	}

	return db.submitMemberUpdate(raftOpRemoveMember, &memberUpdate{Member: m})
}

// AddMember adds a member to the system.
func (db *Database) AddMember(newMember *Member) error {
	if err := db.checkLeader(); err != nil {
		return err
	}
	db.Lock()
	defer db.Unlock()

	mu := &memberUpdate{Member: newMember}
	if cur, err := db.FindMemberByUUID(newMember.UUID); err == nil {
		return &ErrMemberExists{Rank: cur.Rank}
	}

	if newMember.Rank.Equals(NilRank) {
		newMember.Rank = db.data.NextRank
		mu.NextRank = true
	}

	// If the new member is a MS replica, add it as a voter.
	if !common.CmpTcpAddr(db.getReplica(), newMember.Addr) {
		repAddr, _, err := db.checkReplica(newMember.Addr)
		if err != nil {
			return err
		}

		rsi := raft.ServerID(repAddr.String())
		rsa := raft.ServerAddress(repAddr.String())
		if f := db.raft.AddVoter(rsi, rsa, 0, 0); f.Error() != nil {
			return errors.Wrapf(err, "failed to add %q as raft replica", repAddr)
		}
	}

	if err := db.submitMemberUpdate(raftOpAddMember, mu); err != nil {
		return err
	}

	return nil
}

// UpdateMember updates an existing member.
func (db *Database) UpdateMember(m *Member) error {
	if err := db.checkLeader(); err != nil {
		return err
	}
	db.Lock()
	defer db.Unlock()

	_, err := db.FindMemberByUUID(m.UUID)
	if err != nil {
		return err
	}

	return db.submitMemberUpdate(raftOpUpdateMember, &memberUpdate{Member: m})
}

// FindMemberByRank searches the member database by rank. If no
// member is found, an error is returned.
func (db *Database) FindMemberByRank(rank Rank) (*Member, error) {
	if err := db.checkLeader(); err != nil {
		return nil, err
	}
	db.data.RLock()
	defer db.data.RUnlock()

	if m, found := db.data.Members.Ranks[rank]; found {
		return m, nil
	}

	return nil, &ErrMemberNotFound{byRank: &rank}
}

// FindMemberByUUID searches the member database by UUID. If no
// member is found, an error is returned.
func (db *Database) FindMemberByUUID(uuid uuid.UUID) (*Member, error) {
	if err := db.checkLeader(); err != nil {
		return nil, err
	}
	db.data.RLock()
	defer db.data.RUnlock()

	if m, found := db.data.Members.Uuids[uuid]; found {
		return m, nil
	}

	return nil, &ErrMemberNotFound{byUUID: &uuid}
}

// FindMembersByAddr searches the member database by control address. If no
// members are found, an error is returned. This search may return multiple
// members, as a given address may be associated with more than one rank.
func (db *Database) FindMembersByAddr(addr *net.TCPAddr) ([]*Member, error) {
	if err := db.checkLeader(); err != nil {
		return nil, err
	}
	db.data.RLock()
	defer db.data.RUnlock()

	if m, found := db.data.Members.Addrs[addr.String()]; found {
		return m, nil
	}

	return nil, &ErrMemberNotFound{byAddr: addr}
}

// PoolServiceList returns a list of pool services registered
// with the system.
func (db *Database) PoolServiceList() ([]*PoolService, error) {
	if err := db.checkLeader(); err != nil {
		return nil, err
	}
	db.data.RLock()
	defer db.data.RUnlock()

	// NB: This is expensive! We make a copy of the
	// pool services to ensure that they can't be changed
	// elsewhere.
	dbCopy := make([]*PoolService, len(db.data.Pools.Uuids))
	copyIdx := 0
	for _, dbRec := range db.data.Pools.Uuids {
		dbCopy[copyIdx] = new(PoolService)
		*dbCopy[copyIdx] = *dbRec
		copyIdx++
	}
	return dbCopy, nil
}

// FindPoolServiceByUUID searches the pool database by UUID. If no
// pool service is found, an error is returned.
func (db *Database) FindPoolServiceByUUID(uuid uuid.UUID) (*PoolService, error) {
	if err := db.checkLeader(); err != nil {
		return nil, err
	}
	db.data.RLock()
	defer db.data.RUnlock()

	if p, found := db.data.Pools.Uuids[uuid]; found {
		return p, nil
	}

	return nil, &ErrPoolNotFound{byUUID: &uuid}
}

// AddPoolService creates an entry for a new pool service in the pool database.
func (db *Database) AddPoolService(ps *PoolService) error {
	if err := db.checkLeader(); err != nil {
		return err
	}
	db.Lock()
	defer db.Unlock()

	if p, err := db.FindPoolServiceByUUID(ps.PoolUUID); err == nil {
		return errors.Errorf("pool %s already exists", p.PoolUUID)
	}

	if err := db.submitPoolUpdate(raftOpAddPoolService, ps); err != nil {
		return err
	}

	return nil
}

// RemovePoolService removes a pool database entry.
func (db *Database) RemovePoolService(uuid uuid.UUID) error {
	if err := db.checkLeader(); err != nil {
		return err
	}
	db.Lock()
	defer db.Unlock()

	ps, err := db.FindPoolServiceByUUID(uuid)
	if err != nil {
		return errors.Wrapf(err, "failed to retrieve pool %s", uuid)
	}

	if err := db.submitPoolUpdate(raftOpRemovePoolService, ps); err != nil {
		return err
	}

	return nil
}

// UpdatePoolService updates an existing pool database entry.
func (db *Database) UpdatePoolService(ps *PoolService) error {
	if err := db.checkLeader(); err != nil {
		return err
	}
	db.Lock()
	defer db.Unlock()

	_, err := db.FindPoolServiceByUUID(ps.PoolUUID)
	if err != nil {
		return errors.Wrapf(err, "failed to retrieve pool %s", ps.PoolUUID)
	}

	if err := db.submitPoolUpdate(raftOpUpdatePoolService, ps); err != nil {
		return err
	}

	return nil
}
