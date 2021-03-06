// Copyright 2018 New Vector Ltd
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

package sqlite3

import (
	"context"
	"database/sql"

	"github.com/matrix-org/dendrite/common"
	"github.com/matrix-org/dendrite/syncapi/types"
	"github.com/matrix-org/gomatrixserverlib"
)

const outputRoomEventsTopologySchema = `
-- Stores output room events received from the roomserver.
CREATE TABLE IF NOT EXISTS syncapi_output_room_events_topology (
  event_id TEXT PRIMARY KEY,
  topological_position BIGINT NOT NULL,
  stream_position BIGINT NOT NULL,
  room_id TEXT NOT NULL,

	UNIQUE(topological_position, room_id, stream_position)
);
-- The topological order will be used in events selection and ordering
-- CREATE UNIQUE INDEX IF NOT EXISTS syncapi_event_topological_position_idx ON syncapi_output_room_events_topology(topological_position, stream_position, room_id);
`

const insertEventInTopologySQL = "" +
	"INSERT INTO syncapi_output_room_events_topology (event_id, topological_position, room_id, stream_position)" +
	" VALUES ($1, $2, $3, $4)" +
	" ON CONFLICT DO NOTHING"

const selectEventIDsInRangeASCSQL = "" +
	"SELECT event_id FROM syncapi_output_room_events_topology" +
	" WHERE room_id = $1 AND" +
	"(topological_position > $2 AND topological_position < $3) OR" +
	"(topological_position = $4 AND stream_position <= $5)" +
	" ORDER BY topological_position ASC, stream_position ASC LIMIT $6"

const selectEventIDsInRangeDESCSQL = "" +
	"SELECT event_id  FROM syncapi_output_room_events_topology" +
	" WHERE room_id = $1 AND" +
	"(topological_position > $2 AND topological_position < $3) OR" +
	"(topological_position = $4 AND stream_position <= $5)" +
	" ORDER BY topological_position DESC, stream_position DESC LIMIT $6"

const selectPositionInTopologySQL = "" +
	"SELECT topological_position, stream_position FROM syncapi_output_room_events_topology" +
	" WHERE event_id = $1"

const selectMaxPositionInTopologySQL = "" +
	"SELECT MAX(topological_position), stream_position FROM syncapi_output_room_events_topology" +
	" WHERE room_id = $1 ORDER BY stream_position DESC"

const selectEventIDsFromPositionSQL = "" +
	"SELECT event_id FROM syncapi_output_room_events_topology" +
	" WHERE room_id = $1 AND topological_position = $2"

type outputRoomEventsTopologyStatements struct {
	insertEventInTopologyStmt       *sql.Stmt
	selectEventIDsInRangeASCStmt    *sql.Stmt
	selectEventIDsInRangeDESCStmt   *sql.Stmt
	selectPositionInTopologyStmt    *sql.Stmt
	selectMaxPositionInTopologyStmt *sql.Stmt
	selectEventIDsFromPositionStmt  *sql.Stmt
}

func (s *outputRoomEventsTopologyStatements) prepare(db *sql.DB) (err error) {
	_, err = db.Exec(outputRoomEventsTopologySchema)
	if err != nil {
		return
	}
	if s.insertEventInTopologyStmt, err = db.Prepare(insertEventInTopologySQL); err != nil {
		return
	}
	if s.selectEventIDsInRangeASCStmt, err = db.Prepare(selectEventIDsInRangeASCSQL); err != nil {
		return
	}
	if s.selectEventIDsInRangeDESCStmt, err = db.Prepare(selectEventIDsInRangeDESCSQL); err != nil {
		return
	}
	if s.selectPositionInTopologyStmt, err = db.Prepare(selectPositionInTopologySQL); err != nil {
		return
	}
	if s.selectMaxPositionInTopologyStmt, err = db.Prepare(selectMaxPositionInTopologySQL); err != nil {
		return
	}
	if s.selectEventIDsFromPositionStmt, err = db.Prepare(selectEventIDsFromPositionSQL); err != nil {
		return
	}
	return
}

// insertEventInTopology inserts the given event in the room's topology, based
// on the event's depth.
func (s *outputRoomEventsTopologyStatements) insertEventInTopology(
	ctx context.Context, txn *sql.Tx, event *gomatrixserverlib.HeaderedEvent, pos types.StreamPosition,
) (err error) {
	stmt := common.TxStmt(txn, s.insertEventInTopologyStmt)
	_, err = stmt.ExecContext(
		ctx, event.EventID(), event.Depth(), event.RoomID(), pos,
	)
	return
}

// selectEventIDsInRange selects the IDs of events which positions are within a
// given range in a given room's topological order.
// Returns an empty slice if no events match the given range.
func (s *outputRoomEventsTopologyStatements) selectEventIDsInRange(
	ctx context.Context, txn *sql.Tx, roomID string,
	fromPos, toPos, toMicroPos types.StreamPosition,
	limit int, chronologicalOrder bool,
) (eventIDs []string, err error) {
	// Decide on the selection's order according to whether chronological order
	// is requested or not.
	var stmt *sql.Stmt
	if chronologicalOrder {
		stmt = common.TxStmt(txn, s.selectEventIDsInRangeASCStmt)
	} else {
		stmt = common.TxStmt(txn, s.selectEventIDsInRangeDESCStmt)
	}

	// Query the event IDs.
	rows, err := stmt.QueryContext(ctx, roomID, fromPos, toPos, toPos, toMicroPos, limit)
	if err == sql.ErrNoRows {
		// If no event matched the request, return an empty slice.
		return []string{}, nil
	} else if err != nil {
		return
	}

	// Return the IDs.
	var eventID string
	for rows.Next() {
		if err = rows.Scan(&eventID); err != nil {
			return
		}
		eventIDs = append(eventIDs, eventID)
	}

	return
}

// selectPositionInTopology returns the position of a given event in the
// topology of the room it belongs to.
func (s *outputRoomEventsTopologyStatements) selectPositionInTopology(
	ctx context.Context, txn *sql.Tx, eventID string,
) (pos types.StreamPosition, spos types.StreamPosition, err error) {
	stmt := common.TxStmt(txn, s.selectPositionInTopologyStmt)
	err = stmt.QueryRowContext(ctx, eventID).Scan(&pos, &spos)
	return
}

func (s *outputRoomEventsTopologyStatements) selectMaxPositionInTopology(
	ctx context.Context, txn *sql.Tx, roomID string,
) (pos types.StreamPosition, spos types.StreamPosition, err error) {
	stmt := common.TxStmt(txn, s.selectMaxPositionInTopologyStmt)
	err = stmt.QueryRowContext(ctx, roomID).Scan(&pos, &spos)
	return
}

// selectEventIDsFromPosition returns the IDs of all events that have a given
// position in the topology of a given room.
func (s *outputRoomEventsTopologyStatements) selectEventIDsFromPosition(
	ctx context.Context, txn *sql.Tx, roomID string, pos types.StreamPosition,
) (eventIDs []string, err error) {
	// Query the event IDs.
	stmt := common.TxStmt(txn, s.selectEventIDsFromPositionStmt)
	rows, err := stmt.QueryContext(ctx, roomID, pos)
	if err == sql.ErrNoRows {
		// If no event matched the request, return an empty slice.
		return []string{}, nil
	} else if err != nil {
		return
	}
	// Return the IDs.
	var eventID string
	for rows.Next() {
		if err = rows.Scan(&eventID); err != nil {
			return
		}
		eventIDs = append(eventIDs, eventID)
	}
	return
}
