// Copyright 2017 Vector Creations Ltd
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

package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/matrix-org/dendrite/clientapi/jsonerror"
	"github.com/matrix-org/dendrite/clientapi/producers"
	"github.com/matrix-org/dendrite/common/config"
	"github.com/matrix-org/dendrite/roomserver/api"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/util"
	"github.com/sirupsen/logrus"
)

// Send implements /_matrix/federation/v1/send/{txnID}
func Send(
	httpReq *http.Request,
	request *gomatrixserverlib.FederationRequest,
	txnID gomatrixserverlib.TransactionID,
	cfg *config.Dendrite,
	rsAPI api.RoomserverInternalAPI,
	producer *producers.RoomserverProducer,
	eduProducer *producers.EDUServerProducer,
	keys gomatrixserverlib.KeyRing,
	federation *gomatrixserverlib.FederationClient,
) util.JSONResponse {
	t := txnReq{
		context:     httpReq.Context(),
		rsAPI:       rsAPI,
		producer:    producer,
		eduProducer: eduProducer,
		keys:        keys,
		federation:  federation,
	}

	var txnEvents struct {
		PDUs []json.RawMessage       `json:"pdus"`
		EDUs []gomatrixserverlib.EDU `json:"edus"`
	}

	if err := json.Unmarshal(request.Content(), &txnEvents); err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: jsonerror.NotJSON("The request body could not be decoded into valid JSON. " + err.Error()),
		}
	}

	// TODO: Really we should have a function to convert FederationRequest to txnReq
	t.PDUs = txnEvents.PDUs
	t.EDUs = txnEvents.EDUs
	t.Origin = request.Origin()
	t.TransactionID = txnID
	t.Destination = cfg.Matrix.ServerName

	util.GetLogger(httpReq.Context()).Infof("Received transaction %q containing %d PDUs, %d EDUs", txnID, len(t.PDUs), len(t.EDUs))

	resp, err := t.processTransaction()
	switch err.(type) {
	// No error? Great! Send back a 200.
	case nil:
		return util.JSONResponse{
			Code: http.StatusOK,
			JSON: resp,
		}
	// Handle known error cases as we will return a 400 error for these.
	case roomNotFoundError:
	case unmarshalError:
	case verifySigError:
	// Handle unknown error cases. Sending 500 errors back should be a last
	// resort as this can make other homeservers back off sending federation
	// events.
	default:
		util.GetLogger(httpReq.Context()).WithError(err).Error("t.processTransaction failed")
		return jsonerror.InternalServerError()
	}
	// Return a 400 error for bad requests as fallen through from above.
	return util.JSONResponse{
		Code: http.StatusBadRequest,
		JSON: jsonerror.BadJSON(err.Error()),
	}
}

type txnReq struct {
	gomatrixserverlib.Transaction
	context     context.Context
	rsAPI       api.RoomserverInternalAPI
	producer    *producers.RoomserverProducer
	eduProducer *producers.EDUServerProducer
	keys        gomatrixserverlib.JSONVerifier
	federation  txnFederationClient
}

// A subset of FederationClient functionality that txn requires. Useful for testing.
type txnFederationClient interface {
	LookupState(ctx context.Context, s gomatrixserverlib.ServerName, roomID string, eventID string, roomVersion gomatrixserverlib.RoomVersion) (
		res gomatrixserverlib.RespState, err error,
	)
	LookupStateIDs(ctx context.Context, s gomatrixserverlib.ServerName, roomID string, eventID string) (res gomatrixserverlib.RespStateIDs, err error)
	GetEvent(ctx context.Context, s gomatrixserverlib.ServerName, eventID string) (res gomatrixserverlib.Transaction, err error)
	LookupMissingEvents(ctx context.Context, s gomatrixserverlib.ServerName, roomID string, missing gomatrixserverlib.MissingEvents,
		roomVersion gomatrixserverlib.RoomVersion) (res gomatrixserverlib.RespMissingEvents, err error)
}

func (t *txnReq) processTransaction() (*gomatrixserverlib.RespSend, error) {
	results := make(map[string]gomatrixserverlib.PDUResult)

	var pdus []gomatrixserverlib.HeaderedEvent
	for _, pdu := range t.PDUs {
		var header struct {
			RoomID string `json:"room_id"`
		}
		if err := json.Unmarshal(pdu, &header); err != nil {
			util.GetLogger(t.context).WithError(err).Warn("Transaction: Failed to extract room ID from event")
			return nil, unmarshalError{err}
		}
		verReq := api.QueryRoomVersionForRoomRequest{RoomID: header.RoomID}
		verRes := api.QueryRoomVersionForRoomResponse{}
		if err := t.rsAPI.QueryRoomVersionForRoom(t.context, &verReq, &verRes); err != nil {
			util.GetLogger(t.context).WithError(err).Warn("Transaction: Failed to query room version for room", verReq.RoomID)
			return nil, roomNotFoundError{verReq.RoomID}
		}
		event, err := gomatrixserverlib.NewEventFromUntrustedJSON(pdu, verRes.RoomVersion)
		if err != nil {
			util.GetLogger(t.context).WithError(err).Warnf("Transaction: Failed to parse event JSON of event %q", event.EventID())
			return nil, unmarshalError{err}
		}
		if err := gomatrixserverlib.VerifyAllEventSignatures(t.context, []gomatrixserverlib.Event{event}, t.keys); err != nil {
			util.GetLogger(t.context).WithError(err).Warnf("Transaction: Couldn't validate signature of event %q", event.EventID())
			return nil, verifySigError{event.EventID(), err}
		}
		pdus = append(pdus, event.Headered(verRes.RoomVersion))
	}

	// Process the events.
	for _, e := range pdus {
		err := t.processEvent(e.Unwrap(), true)
		if err != nil {
			// If the error is due to the event itself being bad then we skip
			// it and move onto the next event. We report an error so that the
			// sender knows that we have skipped processing it.
			//
			// However if the event is due to a temporary failure in our server
			// such as a database being unavailable then we should bail, and
			// hope that the sender will retry when we are feeling better.
			//
			// It is uncertain what we should do if an event fails because
			// we failed to fetch more information from the sending server.
			// For example if a request to /state fails.
			// If we skip the event then we risk missing the event until we
			// receive another event referencing it.
			// If we bail and stop processing then we risk wedging incoming
			// transactions from that server forever.
			switch err.(type) {
			case roomNotFoundError:
			case *gomatrixserverlib.NotAllowed:
			case missingPrevEventsError:
			default:
				// Any other error should be the result of a temporary error in
				// our server so we should bail processing the transaction entirely.
				return nil, err
			}
			results[e.EventID()] = gomatrixserverlib.PDUResult{
				Error: err.Error(),
			}
			util.GetLogger(t.context).WithError(err).WithField("event_id", e.EventID()).Warn("Failed to process incoming federation event, skipping it.")
		} else {
			results[e.EventID()] = gomatrixserverlib.PDUResult{}
		}
	}

	t.processEDUs(t.EDUs)
	util.GetLogger(t.context).Infof("Processed %d PDUs from transaction %q", len(results), t.TransactionID)
	return &gomatrixserverlib.RespSend{PDUs: results}, nil
}

type roomNotFoundError struct {
	roomID string
}
type unmarshalError struct {
	err error
}
type verifySigError struct {
	eventID string
	err     error
}
type missingPrevEventsError struct {
	eventID string
	err     error
}

func (e roomNotFoundError) Error() string { return fmt.Sprintf("room %q not found", e.roomID) }
func (e unmarshalError) Error() string    { return fmt.Sprintf("unable to parse event: %s", e.err) }
func (e verifySigError) Error() string {
	return fmt.Sprintf("unable to verify signature of event %q: %s", e.eventID, e.err)
}
func (e missingPrevEventsError) Error() string {
	return fmt.Sprintf("unable to get prev_events for event %q: %s", e.eventID, e.err)
}

func (t *txnReq) processEDUs(edus []gomatrixserverlib.EDU) {
	for _, e := range edus {
		switch e.Type {
		case gomatrixserverlib.MTyping:
			// https://matrix.org/docs/spec/server_server/latest#typing-notifications
			var typingPayload struct {
				RoomID string `json:"room_id"`
				UserID string `json:"user_id"`
				Typing bool   `json:"typing"`
			}
			if err := json.Unmarshal(e.Content, &typingPayload); err != nil {
				util.GetLogger(t.context).WithError(err).Error("Failed to unmarshal typing event")
				continue
			}
			if err := t.eduProducer.SendTyping(t.context, typingPayload.UserID, typingPayload.RoomID, typingPayload.Typing, 30*1000); err != nil {
				util.GetLogger(t.context).WithError(err).Error("Failed to send typing event to edu server")
			}
		default:
			util.GetLogger(t.context).WithField("type", e.Type).Warn("unhandled edu")
		}
	}
}

func (t *txnReq) processEvent(e gomatrixserverlib.Event, isInboundTxn bool) error {
	prevEventIDs := e.PrevEventIDs()
	util.GetLogger(t.context).Infof("processEvent %s with prev_events %v", e.EventID(), prevEventIDs)

	// Fetch the state needed to authenticate the event.
	needed := gomatrixserverlib.StateNeededForAuth([]gomatrixserverlib.Event{e})
	stateReq := api.QueryStateAfterEventsRequest{
		RoomID:       e.RoomID(),
		PrevEventIDs: prevEventIDs,
		StateToFetch: needed.Tuples(),
	}
	var stateResp api.QueryStateAfterEventsResponse
	if err := t.rsAPI.QueryStateAfterEvents(t.context, &stateReq, &stateResp); err != nil {
		return err
	}
	util.GetLogger(t.context).Infof("processEvent %s stateResp.PrevEventsExist: %v", e.EventID(), stateResp.PrevEventsExist)

	if !stateResp.RoomExists {
		// TODO: When synapse receives a message for a room it is not in it
		// asks the remote server for the state of the room so that it can
		// check if the remote server knows of a join "m.room.member" event
		// that this server is unaware of.
		// However generally speaking we should reject events for rooms we
		// aren't a member of.
		return roomNotFoundError{e.RoomID()}
	}

	if !stateResp.PrevEventsExist {
		return t.processEventWithMissingState(e, stateResp.RoomVersion, isInboundTxn)
	}

	// Check that the event is allowed by the state at the event.
	var events []gomatrixserverlib.Event
	for _, headeredEvent := range stateResp.StateEvents {
		events = append(events, headeredEvent.Unwrap())
		util.GetLogger(t.context).Infof("Room state: %s -> %s", headeredEvent.Type(), string(headeredEvent.Content()))
	}
	if err := checkAllowedByState(e, events); err != nil {
		return err
	}

	// TODO: Check that the roomserver has a copy of all of the auth_events.
	// TODO: Check that the event is allowed by its auth_events.

	// pass the event to the roomserver
	_, err := t.producer.SendEvents(
		t.context,
		[]gomatrixserverlib.HeaderedEvent{
			e.Headered(stateResp.RoomVersion),
		},
		api.DoNotSendToOtherServers,
		nil,
	)
	return err
}

func checkAllowedByState(e gomatrixserverlib.Event, stateEvents []gomatrixserverlib.Event) error {
	authUsingState := gomatrixserverlib.NewAuthEvents(nil)
	for i := range stateEvents {
		err := authUsingState.AddEvent(&stateEvents[i])
		if err != nil {
			return err
		}
	}
	return gomatrixserverlib.Allowed(e, &authUsingState)
}

func (t *txnReq) processEventWithMissingState(e gomatrixserverlib.Event, roomVersion gomatrixserverlib.RoomVersion, isInboundTxn bool) error {
	// We are missing the previous events for this events.
	// This means that there is a gap in our view of the history of the
	// room. There two ways that we can handle such a gap:
	//   1) We can fill in the gap using /get_missing_events
	//   2) We can leave the gap and request the state of the room at
	//      this event from the remote server using either /state_ids
	//      or /state.
	// Synapse will attempt to do 1 and if that fails or if the gap is
	// too large then it will attempt 2.
	// Synapse will use /state_ids if possible since usually the state
	// is largely unchanged and it is more efficient to fetch a list of
	// event ids and then use /event to fetch the individual events.
	// However not all version of synapse support /state_ids so you may
	// need to fallback to /state.

	// Attempt to fill in the gap using /get_missing_events
	// This will either:
	// - fill in the gap completely then process event `e` returning no backwards extremity
	// - fail to fill in the gap and tell us to terminate the transaction err=not nil
	// - fail to fill in the gap and tell us to fetch state at the new backwards extremity, and to not terminate the transaction
	backwardsExtremity, err := t.getMissingEvents(e, roomVersion, isInboundTxn)
	if err != nil {
		return err
	}
	if backwardsExtremity == nil {
		// we filled in the gap!
		return nil
	}

	// fetch the state BEFORE the event then check that the event is allowed
	respState, haveEventIDs, err := t.lookupStateAfterEvent(roomVersion, *backwardsExtremity)
	if err != nil {
		return err
	}
	fmt.Println("Calcuated lookupStateAfterEvent")

	// pass the event along with the state to the roomserver using a background context so we don't
	// needlessly expire
	return t.producer.SendEventWithState(context.Background(), respState, e.Headered(roomVersion), haveEventIDs)
}

// lookupStateAfterEvent returns the room state after the event e, which is all the states before e resolved via state resolution
// then having e applied to the resulting state.
func (t *txnReq) lookupStateAfterEvent(roomVersion gomatrixserverlib.RoomVersion, e gomatrixserverlib.Event) (*gomatrixserverlib.RespState, map[string]bool, error) {
	// de-dupe all the events
	authEvents := make(map[string]*gomatrixserverlib.Event)
	stateEvents := make(map[string]*gomatrixserverlib.Event)
	haveEventIDs := make(map[string]bool)
	for _, prevEventID := range e.PrevEventIDs() {
		// don't do auth checks on this RespState as we're just interested in grabbing state/auth events and putting it into the pot
		respState, haveIDs, err := t.lookupStateBeforeEvent(roomVersion, false, e.RoomID(), prevEventID)
		if err != nil {
			return nil, nil, err
		}
		for i := range respState.StateEvents {
			stateEvents[respState.StateEvents[i].EventID()] = &respState.StateEvents[i]
		}
		for i := range respState.AuthEvents {
			authEvents[respState.AuthEvents[i].EventID()] = &respState.AuthEvents[i]
		}
		for id := range haveIDs {
			haveEventIDs[id] = true
		}
		// fetch the event we're missing and add it to the pile
		h, err := t.lookupEvent(roomVersion, prevEventID)
		if err != nil {
			return nil, nil, err
		}
		if h.StateKey() != nil {
			he := h.Unwrap()
			stateEvents[h.EventID()] = &he
		}
	}
	authEventList := make([]gomatrixserverlib.Event, len(authEvents))
	i := 0
	for _, ev := range authEvents {
		authEventList[i] = *ev
		i++
	}
	stateEventList := make([]gomatrixserverlib.Event, len(stateEvents))
	i = 0
	for _, ev := range stateEvents {
		stateEventList[i] = *ev
		i++
	}
	resolvedStateEvents, err := gomatrixserverlib.ResolveConflicts(roomVersion, stateEventList, authEventList)
	if err != nil {
		return nil, nil, err
	}
	// apply the current event
	if err = checkAllowedByState(e, resolvedStateEvents); err != nil {
		return nil, nil, err
	}
	// roll forward state if this event is a state event
	if e.StateKey() != nil {
		for i := range resolvedStateEvents {
			if resolvedStateEvents[i].Type() == e.Type() && resolvedStateEvents[i].StateKeyEquals(*e.StateKey()) {
				resolvedStateEvents[i] = e
				break
			}
		}
	}
	for _, s := range resolvedStateEvents {
		util.GetLogger(t.context).Infof("resolved: %s -> %s", s.Type(), string(s.Content()))
	}
	for _, s := range authEventList {
		util.GetLogger(t.context).Infof("authEventList: %s -> %s", s.Type(), string(s.Content()))
	}

	resp := &gomatrixserverlib.RespState{
		AuthEvents:  authEventList,
		StateEvents: resolvedStateEvents,
	}
	if err = resp.Check(t.context, t.keys); err != nil {
		return nil, nil, fmt.Errorf("lookupStateAfterEvent: resolved state is not valid: %w", err)
	}

	return resp, haveEventIDs, nil
}

// lookuptStateBeforeEvent returns the room state before the event e, which is just /state_ids and/or /state depending on what
// the server supports.
func (t *txnReq) lookupStateBeforeEvent(roomVersion gomatrixserverlib.RoomVersion, doAuthCheck bool, roomID, eventID string) (
	respState *gomatrixserverlib.RespState, haveEventIDs map[string]bool, err error) {

	util.GetLogger(t.context).Infof("lookupStateBeforeEvent %s", eventID)
	// It's entirely possible that we know this state, as QueryStateAfterEventsRequest only returns success if ALL prev_events
	// exist, so query the roomserver for the state with just this prev event
	stateReq := api.QueryStateAfterEventsRequest{
		RoomID:       roomID,
		StateToFetch: nil, // TODO: do we need everything?
		PrevEventIDs: []string{eventID},
	}
	var stateResp api.QueryStateAfterEventsResponse
	if err = t.rsAPI.QueryStateAfterEvents(t.context, &stateReq, &stateResp); err != nil || stateResp.StateEvents == nil {
		util.GetLogger(t.context).WithError(err).Warnf("Failed to lookup state before event %s via roomserver - asking remote", eventID)
		// fallthrough to remote lookup
	} else {
		util.GetLogger(t.context).Infof("lookupStateBeforeEvent %s returned locally", eventID)
		// we have all the events
		haveEvents := make(map[string]*gomatrixserverlib.HeaderedEvent)
		haveEventIDs = make(map[string]bool)
		for i, ev := range stateResp.StateEvents {
			haveEventIDs[ev.EventID()] = true
			haveEvents[ev.EventID()] = &stateResp.StateEvents[i]
		}
		var authEvents []gomatrixserverlib.Event
		for _, ev := range stateResp.StateEvents {
			for _, ae := range ev.AuthEventIDs() {
				aev, ok := haveEvents[ae]
				if ok {
					authEvents = append(authEvents, aev.Unwrap())
				}
			}
		}

		respState = &gomatrixserverlib.RespState{
			AuthEvents:  authEvents,
			StateEvents: gomatrixserverlib.UnwrapEventHeaders(stateResp.StateEvents),
		}
		return
	}

	// Attempt to fetch the missing state using /state_ids and /events
	respState, haveEventIDs, err = t.lookupMissingStateViaStateIDs(roomID, eventID, doAuthCheck, roomVersion)
	if err != nil {
		// Fallback to /state
		util.GetLogger(t.context).WithError(err).Warn("lookupStateBeforeEvent failed to /state_ids, falling back to /state")
		respState, err = t.lookupMissingStateViaState(roomID, eventID, roomVersion)
	}
	return
}

// getMissingEvents returns a nil backwardsExtremity if missing events were fetched and handled, else returns the new backwards extremity which we should
// begin from. Returns an error only if we should terminate the transaction which initiated /get_missing_events
// This function recursively calls txnReq.processEvent with the missing events, which will be processed before this function returns.
// This means that we may recursively call this function, as we spider back up prev_events to the min depth.
func (t *txnReq) getMissingEvents(e gomatrixserverlib.Event, roomVersion gomatrixserverlib.RoomVersion, isInboundTxn bool) (backwardsExtremity *gomatrixserverlib.Event, err error) {
	if !isInboundTxn {
		// we've recursed here, so just take a state snapshot please!
		fmt.Println("backwards extremity is now ", e.EventID())
		return &e, nil
	}
	logger := util.GetLogger(t.context).WithField("event_id", e.EventID()).WithField("room_id", e.RoomID())
	needed := gomatrixserverlib.StateNeededForAuth([]gomatrixserverlib.Event{e})
	// query latest events (our trusted forward extremities)
	req := api.QueryLatestEventsAndStateRequest{
		RoomID:       e.RoomID(),
		StateToFetch: needed.Tuples(),
	}
	var res api.QueryLatestEventsAndStateResponse
	if err = t.rsAPI.QueryLatestEventsAndState(t.context, &req, &res); err != nil {
		logger.WithError(err).Warn("Failed to query latest events")
		return &e, nil
	}
	latestEvents := make([]string, len(res.LatestEvents))
	for i := range res.LatestEvents {
		latestEvents[i] = res.LatestEvents[i].EventID
	}
	// this server just sent us an event for which we do not know its prev_events - ask that server for those prev_events.
	missingResp, err := t.federation.LookupMissingEvents(t.context, t.Origin, e.RoomID(), gomatrixserverlib.MissingEvents{
		Limit: 20,
		// synapse uses the min depth they've ever seen in that room
		MinDepth: int(res.Depth) - 20,
		// The latest event IDs that the sender already has. These are skipped when retrieving the previous events of latest_events.
		EarliestEvents: latestEvents,
		// The event IDs to retrieve the previous events for.
		LatestEvents: []string{e.EventID()},
	}, roomVersion)

	// security: how we handle failures depends on whether or not this event will become the new forward extremity for the room.
	// There's 2 scenarios to consider:
	// - Case A: We got pushed an event and are now fetching missing prev_events. (isInboundTxn=true)
	// - Case B: We are fetching missing prev_events already and now fetching some more  (isInboundTxn=false)
	// In Case B, we know for sure that the event we are currently processing will not become the new forward extremity for the room,
	// as it was called in response to an inbound txn which had it as a prev_event.
	// In Case A, the event is a forward extremity, and could eventually become the _only_ forward extremity in the room. This is bad
	// because it means we would trust the state at that event to be the state for the entire room, and allows rooms to be hijacked.
	// https://github.com/matrix-org/synapse/pull/3456
	// https://github.com/matrix-org/synapse/blob/229eb81498b0fe1da81e9b5b333a0285acde9446/synapse/handlers/federation.py#L335
	// For now, we do not allow Case B, so reject the event.
	if err != nil {
		logger.WithError(err).Errorf(
			"%s pushed us an event but couldn't give us details about prev_events via /get_missing_events - dropping this event until it can",
			t.Origin,
		)
		return nil, missingPrevEventsError{
			eventID: e.EventID(),
			err:     err,
		}
	}
	logger.Infof("get_missing_events returned %d events", len(missingResp.Events))

	// topologically sort and sanity check that we are making forward progress
	newEvents := gomatrixserverlib.ReverseTopologicalOrdering(missingResp.Events, gomatrixserverlib.TopologicalOrderByPrevEvents)
	shouldHaveSomeEventIDs := e.PrevEventIDs()
	hasPrevEvent := false
Event:
	for _, pe := range shouldHaveSomeEventIDs {
		for _, ev := range newEvents {
			if ev.EventID() == pe {
				hasPrevEvent = true
				break Event
			}
		}
	}
	if !hasPrevEvent {
		err = fmt.Errorf("called /get_missing_events but server %s didn't return any prev_events with IDs %v", t.Origin, shouldHaveSomeEventIDs)
		logger.WithError(err).Errorf(
			"%s pushed us an event but couldn't give us details about prev_events via /get_missing_events - dropping this event until it can",
			t.Origin,
		)
		return nil, missingPrevEventsError{
			eventID: e.EventID(),
			err:     err,
		}
	}
	// process the missing events then the event which started this whole thing
	for _, ev := range append(newEvents, e) {
		err := t.processEvent(ev, false)
		if err != nil {
			return nil, err
		}
	}

	// we processed everything!
	return nil, nil
}

func (t *txnReq) lookupMissingStateViaState(roomID, eventID string, roomVersion gomatrixserverlib.RoomVersion) (
	respState *gomatrixserverlib.RespState, err error) {
	state, err := t.federation.LookupState(t.context, t.Origin, roomID, eventID, roomVersion)
	if err != nil {
		return nil, err
	}
	// Check that the returned state is valid.
	if err := state.Check(t.context, t.keys); err != nil {
		return nil, err
	}
	return &state, nil
}

func (t *txnReq) lookupMissingStateViaStateIDs(roomID, eventID string, doAuthCheck bool, roomVersion gomatrixserverlib.RoomVersion) (
	*gomatrixserverlib.RespState, map[string]bool, error) {
	util.GetLogger(t.context).Infof("lookupMissingStateViaStateIDs %s", eventID)
	// fetch the state event IDs at the time of the event
	stateIDs, err := t.federation.LookupStateIDs(t.context, t.Origin, roomID, eventID)
	if err != nil {
		return nil, nil, err
	}

	// fetch as many as we can from the roomserver, do them as 2 calls rather than
	// 1 to try to reduce the number of parameters in the bulk query this will use
	haveEventMap := make(map[string]*gomatrixserverlib.HeaderedEvent, len(stateIDs.StateEventIDs))
	haveEventIDs := make(map[string]bool)
	for _, eventList := range [][]string{stateIDs.StateEventIDs, stateIDs.AuthEventIDs} {
		queryReq := api.QueryEventsByIDRequest{
			EventIDs: eventList,
		}
		var queryRes api.QueryEventsByIDResponse
		if err = t.rsAPI.QueryEventsByID(t.context, &queryReq, &queryRes); err != nil {
			return nil, nil, err
		}
		// allow indexing of current state by event ID
		for i := range queryRes.Events {
			haveEventMap[queryRes.Events[i].EventID()] = &queryRes.Events[i]
			haveEventIDs[queryRes.Events[i].EventID()] = true
		}
	}

	// work out which auth/state IDs are missing
	wantIDs := append(stateIDs.StateEventIDs, stateIDs.AuthEventIDs...)
	missing := make(map[string]bool)
	for _, sid := range wantIDs {
		if _, ok := haveEventMap[sid]; !ok {
			missing[sid] = true
		}
	}
	util.GetLogger(t.context).WithFields(logrus.Fields{
		"missing":           len(missing),
		"event_id":          eventID,
		"room_id":           roomID,
		"already_have":      len(haveEventMap),
		"total_state":       len(stateIDs.StateEventIDs),
		"total_auth_events": len(stateIDs.AuthEventIDs),
	}).Info("Fetching missing state at event")

	for missingEventID := range missing {
		var h *gomatrixserverlib.HeaderedEvent
		h, err = t.lookupEvent(roomVersion, missingEventID)
		if err != nil {
			return nil, nil, err
		}
		haveEventMap[h.EventID()] = h
	}
	resp, err := t.createRespStateFromStateIDs(stateIDs, doAuthCheck, haveEventMap)
	return resp, haveEventIDs, err
}

func (t *txnReq) createRespStateFromStateIDs(stateIDs gomatrixserverlib.RespStateIDs, doAuthCheck bool, haveEventMap map[string]*gomatrixserverlib.HeaderedEvent) (
	*gomatrixserverlib.RespState, error) {
	// create a RespState response using the response to /state_ids as a guide
	respState := gomatrixserverlib.RespState{
		AuthEvents:  make([]gomatrixserverlib.Event, len(stateIDs.AuthEventIDs)),
		StateEvents: make([]gomatrixserverlib.Event, len(stateIDs.StateEventIDs)),
	}
	var roomVer gomatrixserverlib.RoomVersion

	for i := range stateIDs.StateEventIDs {
		ev, ok := haveEventMap[stateIDs.StateEventIDs[i]]
		if !ok {
			return nil, fmt.Errorf("missing state event %s", stateIDs.StateEventIDs[i])
		}
		respState.StateEvents[i] = ev.Unwrap()
		roomVer = ev.RoomVersion
	}
	for i := range stateIDs.AuthEventIDs {
		ev, ok := haveEventMap[stateIDs.AuthEventIDs[i]]
		if !ok {
			return nil, fmt.Errorf("missing auth event %s", stateIDs.AuthEventIDs[i])
		}
		respState.AuthEvents[i] = ev.Unwrap()
	}
	// Check that the returned state is valid.
retryCheck:
	if err := respState.Check(t.context, t.keys); err != nil {
		switch missing := err.(type) {
		case gomatrixserverlib.MissingAuthEventError:
			// An auth event was missing so let's look up that event over federation
			var newEv *gomatrixserverlib.HeaderedEvent
			newEv, err = t.lookupEvent(roomVer, missing.AuthEventID)
			if err != nil {
				// we can't find this event, fail
				return nil, fmt.Errorf("missing auth event %s and cannot find it: %w", missing.AuthEventID, err)
			}
			respState.AuthEvents = append(respState.AuthEvents, newEv.Unwrap())
			goto retryCheck
		}
		if doAuthCheck {
			return nil, err
		} else {
			return &respState, nil
		}
	}
	return &respState, nil
}

func (t *txnReq) lookupEvent(roomVersion gomatrixserverlib.RoomVersion, missingEventID string) (*gomatrixserverlib.HeaderedEvent, error) {
	txn, err := t.federation.GetEvent(t.context, t.Origin, missingEventID)
	if err != nil || len(txn.PDUs) == 0 {
		util.GetLogger(t.context).WithError(err).WithField("event_id", missingEventID).Warn("failed to get missing /event for event ID")
		return nil, err
	}
	pdu := txn.PDUs[0]
	var event gomatrixserverlib.Event
	event, err = gomatrixserverlib.NewEventFromUntrustedJSON(pdu, roomVersion)
	if err != nil {
		util.GetLogger(t.context).WithError(err).Warnf("Transaction: Failed to parse event JSON of event %q", event.EventID())
		return nil, unmarshalError{err}
	}
	if err = gomatrixserverlib.VerifyAllEventSignatures(t.context, []gomatrixserverlib.Event{event}, t.keys); err != nil {
		util.GetLogger(t.context).WithError(err).Warnf("Transaction: Couldn't validate signature of event %q", event.EventID())
		return nil, verifySigError{event.EventID(), err}
	}
	h := event.Headered(roomVersion)
	return &h, nil
}
