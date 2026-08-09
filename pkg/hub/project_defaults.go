// Copyright 2026 Google LLC
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

package hub

import (
	"log/slog"
	"os"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// EnvProjectDefaultScratchpad opts out of the default scratchpad shared dir in
// file/SQLite mode, where OperationalSettings is not wired and both the
// project_defaults DB row and the admin API are unavailable. Accepts any value
// strconv.ParseBool understands. Ignored in postgres mode, where the DB row is
// the source of truth.
const EnvProjectDefaultScratchpad = "SCION_PROJECT_DEFAULT_SCRATCHPAD"

// defaultProjectSharedDirs returns the hub-configured default shared dirs
// for new projects. Returns a scratchpad shared dir when enabled (the
// compiled default), or nil when the operator has explicitly disabled it.
//
// Thread-safe: reads from OperationalSettings under its internal lock.
func (s *Server) defaultProjectSharedDirs() []api.SharedDir {
	enabled := true // compiled default: ON

	if ops := s.GetOperationalSettings(); ops != nil {
		enabled = ops.ProjectDefaultScratchpad()
	} else if raw, ok := os.LookupEnv(EnvProjectDefaultScratchpad); ok {
		// File/SQLite mode: ops is nil, so the DB row and the admin API cannot
		// reach this default. Honor an explicit env opt-out instead. A value we
		// cannot parse keeps the compiled default rather than guessing.
		if v, err := strconv.ParseBool(raw); err == nil {
			enabled = v
		} else {
			slog.Warn("Ignoring unparseable "+EnvProjectDefaultScratchpad,
				"value", raw, "error", err)
		}
	}

	if !enabled {
		return nil
	}
	return []api.SharedDir{{Name: "scratchpad"}}
}
