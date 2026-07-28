package server

import (
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/relayproto"
)

func targetFromLegacy(target relayproto.SessionRef) hprp.Target {
	return hprp.Target{MachineID: target.MachineID, SlotID: target.PaneID, SessionID: target.OccupantHash}
}

func targetToLegacy(target hprp.Target, localIndex int) relayproto.SessionRef {
	return relayproto.SessionRef{
		MachineID: target.MachineID, LocalIndex: localIndex,
		PaneID: target.SlotID, OccupantHash: target.SessionID,
	}
}

func snapshotFromLegacy(snapshot relayproto.SessionSnapshot) hprp.SessionSnapshot {
	sessions := make([]hprp.Session, len(snapshot.Sessions))
	for index, current := range snapshot.Sessions {
		sessions[index] = hprp.Session{
			SlotID: current.PaneID, SessionID: current.OccupantHash,
			Display: hprp.SessionDisplay{
				Index: current.LocalIndex, Agent: current.Agent, DisplayAgent: current.DisplayAgent,
				Workspace: current.Workspace, Tab: current.Tab, Title: current.Title,
			},
			Status: current.Status,
		}
	}
	return hprp.SessionSnapshot{Sequence: snapshot.Sequence, Sessions: sessions}
}
