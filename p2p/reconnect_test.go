/**
 *  @file
 *  @copyright defined in aergo/LICENSE.txt
 */

package p2p

import (
	"fmt"
	"testing"
	"time"

	"github.com/aergoio/aergo-lib/log"
	peer "github.com/libp2p/go-libp2p-peer"
	"github.com/stretchr/testify/mock"
)

var dummyPeerID peer.ID
var dummyPeerID2 peer.ID
var dummyPeerID3 peer.ID

func init() {
	dummyPeerID, _ = peer.IDB58Decode("16Uiu2HAkvvhjxVm2WE9yFBDdPQ9qx6pX9taF6TTwDNHs8VPi1EeR")
	dummyPeerID2, _ = peer.IDB58Decode("16Uiu2HAmFqptXPfcdaCdwipB2fhHATgKGVFVPehDAPZsDKSU7jRm")
	dummyPeerID3, _ = peer.IDB58Decode("16Uiu2HAmU8Wc925gZ5QokM4sGDKjysdPwRCQFoYobvoVnyutccCD")
}
func Test_reconnectRunner_runReconnect(t *testing.T) {
	logger := log.NewLogger("test.p2p")
	// TODO: is it ok that this global var can be changed.
	durations = []time.Duration{
		time.Millisecond * 100,
		time.Millisecond * 120,
		time.Millisecond * 130,
		time.Millisecond * 150,
	}
	trials := len(durations)
	maxTrial = trials
	mockPm := &MockP2PService{}
	dummyPeer := &RemotePeer{}
	mockPm.On("GetPeer", mock.MatchedBy(func(ID peer.ID) bool { return ID == dummyPeerID })).Return(nil, false)
	mockPm.On("AddNewPeer", mock.AnythingOfType("p2p.PeerMeta"))
	mockPm2 := &MockP2PService{}
	mockPm2.On("GetPeer", mock.MatchedBy(func(ID peer.ID) bool { return ID != dummyPeerID })).Return(dummyPeer, true)
	mockPm2.On("AddNewPeer", mock.AnythingOfType("p2p.PeerMeta"))
	mockPm3 := &MockP2PService{}
	mockPm3.On("GetPeer", mock.MatchedBy(func(ID peer.ID) bool { return ID != dummyPeerID })).Return(nil, false).Times(2)
	mockPm3.On("GetPeer", mock.MatchedBy(func(ID peer.ID) bool { return ID != dummyPeerID })).Return(dummyPeer, true).Once()
	mockPm3.On("AddNewPeer", mock.AnythingOfType("p2p.PeerMeta"))

	dummyRM := NewReconnectManager(log.NewLogger("test.p2p"))

	tests := []struct {
		name        string
		pm          *MockP2PService
		meta        PeerMeta
		lookupCount int
		addCount    int
	}{
		{"t1", mockPm2, PeerMeta{ID: "dgewge"}, 1, 0},
		{"t2", mockPm3, PeerMeta{ID: "dgewge"}, 3, 2},
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := newReconnectRunner(tt.meta, dummyRM, tt.pm, logger)
			rr.runJob()
			tt.pm.AssertNumberOfCalls(t, "GetPeer", tt.lookupCount)
			tt.pm.AssertNumberOfCalls(t, "AddNewPeer", tt.addCount)

		})
	}

	// testb infinity
	rr := newReconnectRunner(PeerMeta{ID: dummyPeerID}, dummyRM, mockPm, logger)
	dummyRM.jobs[dummyPeerID] = rr
	go func() {
		time.Sleep(time.Second)
		dummyRM.CancelJob(dummyPeerID)
	}()
	rr.runJob()

}

func Test_generateExpDuration(t *testing.T) {
	tests := []struct {
		name     string
		initSecs int
		inc      float64
		count    int
		want     int
	}{
		{"T0", 2, 0.6, 10, 10},
		{"T1", 10, 0.6, 10, 10},
		{"T1", 20, 0.6, 15, 15},
		{"T2", 20, 0.75, 15, 15},
		// TODO: Add test cases.
	}
	prev := time.Nanosecond
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fmt.Println("Testing ", tt.name)
			got := generateExpDuration(tt.initSecs, tt.inc, tt.count)
			fmt.Printf("Finally : %v \n", got)
			if len(got) != tt.want {
				t.Errorf("generateExpDuration() = %v, want %v", len(got), tt.want)
			}
			if prev >= got[len(got)-1] {
				t.Errorf("unexpected last value %v ", got[len(got)-1])
			}

		})
	}
}
