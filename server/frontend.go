// Copyright 2014 The Dename Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy of
// the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations under
// the License.

package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	. "github.com/andres-erbsen/dename/protocol"
	"github.com/gogo/protobuf/proto"
	"github.com/syndtr/goleveldb/leveldb"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

type frontend struct {
	server *server
	sync.Mutex
	hasOperations     chan struct{}
	inviteMacKey      []byte
	inviteMutex       sync.Mutex
	profileOperations []*SignedProfileOperation
	waitOps           map[string]chan struct{}

	stop     chan struct{}
	waitStop *sync.WaitGroup
}

func NewFrontend(inviteMacKey []byte) *frontend {
	return &frontend{
		hasOperations: make(chan struct{}, 1),
		inviteMacKey:  inviteMacKey,
		waitOps:       make(map[string]chan struct{}),
	}
}

// caller MUST call fe.waitStop.Add(1) first
func (fe *frontend) listenForClients(ln net.Listener) {
	defer fe.waitStop.Done()
	defer ln.Close()
	go func() {
		<-fe.stop
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		select {
		case <-fe.stop:
			return
		default:
			if err != nil {
				log.Printf("accept client: %s", err)
				continue
			}
		}
		fe.waitStop.Add(1)
		go func(conn net.Conn) {
			err := readHandleLoop(conn, 4<<10, fe.handleClient, fe.stop)
			if err != nil && err != io.EOF {
				log.Printf("frontend on %v: %v", conn.RemoteAddr(), err)
			}
			fe.waitStop.Done()
		}(conn)
	}
}

func (fe *frontend) handleClient(msg []byte, conn net.Conn) (err error) {
	rq := new(ClientMessage)
	err = proto.Unmarshal(Unpad(msg), rq)
	if err != nil {
		conn.Close()
		return
	}
	padTo := int(rq.GetPadReplyTo())
	if padTo > 4<<10 {
		conn.Close()
		return
	}
	_, err = conn.Write(Frame(Pad(PBEncode(fe.handleRequest(rq)), padTo)))
	if err != nil {
		conn.Close()
		return
	}
	return nil
}

func (fe *frontend) handleRequest(rq *ClientMessage) (reply *ClientReply) {
	var err error
	reply = new(ClientReply)
	if rq.PeekState != nil || rq.ResolveName != nil {
		fe.server.state.RLock()
		if rq.PeekState != nil {
			for _, msg := range fe.server.state.confirmations {
				reply.StateConfirmations = append(reply.StateConfirmations, msg.SignedServerMessage)
			}
		}
		if rq.ResolveName != nil {
			_, _, reply.LookupNodes = resolve(fe.server.state.merklemap, rq.ResolveName)
		}
		fe.server.state.RUnlock()
	}
	if rq.ModifyProfile != nil {
		if rq.ModifyProfile.OldProfileSignature == nil { // new profile being created
			if len(rq.InviteCode) != 16 {
				reply.Status = ClientReply_INVITE_INVALID.Enum()
				err = fmt.Errorf("Expected invite code of length 16 but got length %d", len(rq.InviteCode))
				return
			}
			if fe.inviteMacKey == nil {
				err = fmt.Errorf("not handling registrations")
				reply.Status = ClientReply_REGISTRATION_DISABLED.Enum()
				return
			}
			mac := hmac.New(sha256.New, fe.inviteMacKey)
			mac.Write(rq.InviteCode[:8])
			if !hmac.Equal(mac.Sum(nil)[:8], rq.InviteCode[8:]) {
				err = fmt.Errorf("Invalid invite code")
				reply.Status = ClientReply_INVITE_INVALID.Enum()
				return
			}
			icode := append([]byte{'I'}, rq.InviteCode...)
			fe.inviteMutex.Lock()
			if _, err := fe.server.db.Get(icode, nil); err == nil {
				fe.inviteMutex.Unlock()
				err = fmt.Errorf("This invite code has already been used")
				reply.Status = ClientReply_INVITE_USED.Enum()
				return
			} else if err != leveldb.ErrNotFound {
				panic(err)
			}
			if err = fe.server.db.Put(icode, []byte{}, nil); err != nil {
				panic(err)
			}
			fe.inviteMutex.Unlock()
		}
		fe.server.state.RLock()
		m := fe.server.state.merklemap
		_, _, err = validateOperation(m, rq.ModifyProfile, uint64(time.Now().Unix()))
		if err != nil {
			fe.server.state.RUnlock()
			reply.Status = ClientReply_NOT_AUTHORIZED.Enum()
			return
		}
		fe.server.state.RUnlock()
		fe.Lock()
		fe.profileOperations = append(fe.profileOperations, rq.ModifyProfile)
		var ch chan struct{}
		var present bool
		if ch, present = fe.waitOps[string(rq.ModifyProfile.ProfileOperation)]; !present {
			ch = make(chan struct{})
			fe.waitOps[string(rq.ModifyProfile.ProfileOperation)] = ch
		}
		select {
		case fe.hasOperations <- struct{}{}:
		default:
		}
		fe.Unlock()
		<-ch
	}
	return
}

func (fe *frontend) GetOperations() *SignedServerMessage_ServerMessage_OperationsT {
	ret := new(SignedServerMessage_ServerMessage_OperationsT)
	ret.Seed = make([]byte, 16)
	if _, err := rand.Read(ret.Seed); err != nil {
		panic(err)
	}
	t := uint64(time.Now().Unix())
	ret.Time = &t

	fe.Lock()
	ret.ProfileOperations = fe.profileOperations
	fe.profileOperations = make([]*SignedProfileOperation, 0, len(ret.ProfileOperations))
	fe.Unlock()
	return ret
}

func (fe *frontend) DoneWith(ops []*SignedProfileOperation) {
	fe.Lock()
	for _, op := range ops {
		if ch, ok := fe.waitOps[string(op.ProfileOperation)]; ok {
			close(ch)
			delete(fe.waitOps, string(op.ProfileOperation))
		}
	}
	fe.Unlock()
}
