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
	"net/http"

	"github.com/gorilla/mux"
	"github.com/matrix-org/dendrite/clientapi/auth/authtypes"
	"github.com/matrix-org/dendrite/clientapi/auth/storage/devices"
	"github.com/matrix-org/dendrite/common"
	"github.com/matrix-org/dendrite/syncapi/storage"
	"github.com/matrix-org/dendrite/syncapi/sync"
	"github.com/matrix-org/util"
	opentracing "github.com/opentracing/opentracing-go"
)

const pathPrefixR0 = "/_matrix/client/r0"

// Setup configures the given mux with sync-server listeners
func Setup(
	apiMux *mux.Router, srp *sync.RequestPool, syncDB *storage.SyncServerDatabase, deviceDB *devices.Database,
	tracer opentracing.Tracer,
) {
	r0mux := apiMux.PathPrefix(pathPrefixR0).Subrouter()

	r0mux.Handle("/sync", common.MakeAuthAPI(tracer, "sync", deviceDB, func(req *http.Request, device *authtypes.Device) util.JSONResponse {
		return srp.OnIncomingSyncRequest(req, device)
	})).Methods("GET", "OPTIONS")

	r0mux.Handle("/rooms/{roomID}/state", common.MakeAuthAPI(tracer, "room_state", deviceDB, func(req *http.Request, device *authtypes.Device) util.JSONResponse {
		vars := mux.Vars(req)
		return OnIncomingStateRequest(req, syncDB, vars["roomID"])
	})).Methods("GET", "OPTIONS")

	r0mux.Handle("/rooms/{roomID}/state/{type}", common.MakeAuthAPI(tracer, "room_state", deviceDB, func(req *http.Request, device *authtypes.Device) util.JSONResponse {
		vars := mux.Vars(req)
		return OnIncomingStateTypeRequest(req, syncDB, vars["roomID"], vars["type"], "")
	})).Methods("GET", "OPTIONS")

	r0mux.Handle("/rooms/{roomID}/state/{type}/{stateKey}", common.MakeAuthAPI(tracer, "room_state", deviceDB, func(req *http.Request, device *authtypes.Device) util.JSONResponse {
		vars := mux.Vars(req)
		return OnIncomingStateTypeRequest(req, syncDB, vars["roomID"], vars["type"], vars["stateKey"])
	})).Methods("GET", "OPTIONS")
}
