// Copyright © 2019 The Things Network Foundation, The Things Industries B.V.
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

package shared

import (
	"fmt"

	"go.thethings.network/lorawan-stack/pkg/config"
	"go.thethings.network/lorawan-stack/pkg/events"
	"go.thethings.network/lorawan-stack/pkg/events/redis"
)

// InitializeEvents initializes the event system.
func InitializeEvents(config config.ServiceBase) error {
	switch config.Events.Backend {
	case "internal":
		return nil // this is the default.
	case "redis":
		if !config.Events.Redis.IsZero() {
			events.DefaultPubSub = redis.NewPubSub(config.Events.Redis)
		} else {
			events.DefaultPubSub = redis.NewPubSub(config.Redis)
		}
		return nil
	default:
		return fmt.Errorf("unknown events backend: %s", config.Events.Backend)
	}
}
