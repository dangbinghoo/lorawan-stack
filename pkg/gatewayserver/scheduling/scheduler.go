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

package scheduling

import (
	"context"
	"math"
	"sync"
	"time"

	"go.thethings.network/lorawan-stack/pkg/band"
	"go.thethings.network/lorawan-stack/pkg/errors"
	"go.thethings.network/lorawan-stack/pkg/frequencyplans"
	"go.thethings.network/lorawan-stack/pkg/toa"
	"go.thethings.network/lorawan-stack/pkg/ttnpb"
)

var (
	// QueueDelay indicates the time the gateway needs to recharge the concentrator between items in the queue.
	// This is a conservative value as implemented in the Semtech UDP Packet Forwarder reference implementation,
	// see https://github.com/Lora-net/packet_forwarder/blob/v4.0.1/lora_pkt_fwd/src/jitqueue.c#L39
	QueueDelay = 30 * time.Millisecond

	// ScheduleTimeShort is a short time to send a downlink message to the gateway before it has to be transmitted.
	// This time is comprised of a lower network latency and QueueDelay. This delay is used to block scheduling when the
	// schedule time to the estimated concentrator time is less than this value, see ScheduleAt.
	ScheduleTimeShort = 100*time.Millisecond + QueueDelay

	// ScheduleTimeLong is a long time to send a downlink message to the gateway before it has to be transmitted.
	// This time is comprised of a higher network latency and QueueDelay. This delay is used for pseudo-immediate
	// scheduling, see ScheduleAnytime.
	ScheduleTimeLong = 300*time.Millisecond + QueueDelay
)

// NewScheduler instantiates a new Scheduler for the given frequency plan.
func NewScheduler(ctx context.Context, fp *frequencyplans.FrequencyPlan, enforceDutyCycle bool) (*Scheduler, error) {
	toa := fp.TimeOffAir
	if toa.Duration < QueueDelay {
		toa.Duration = QueueDelay
	}
	s := &Scheduler{
		clock:             &RolloverClock{},
		respectsDwellTime: fp.RespectsDwellTime,
		timeOffAir:        toa,
	}
	if enforceDutyCycle {
		band, err := band.GetByID(fp.BandID)
		if err != nil {
			return nil, err
		}
		for _, subBand := range band.SubBands {
			sb := NewSubBand(ctx, subBand, s.clock, nil)
			s.subBands = append(s.subBands, sb)
		}
	} else {
		sb := NewSubBand(ctx, band.SubBandParameters{
			MinFrequency: 0,
			MaxFrequency: math.MaxUint64,
			DutyCycle:    1,
		}, s.clock, nil)
		s.subBands = append(s.subBands, sb)
	}
	return s, nil
}

// Scheduler is a packet scheduler that takes time conflicts and sub-band restrictions into account.
type Scheduler struct {
	clock             *RolloverClock
	respectsDwellTime func(isDownlink bool, frequency uint64, duration time.Duration) bool
	timeOffAir        frequencyplans.TimeOffAir
	subBands          []*SubBand
	mu                sync.Mutex
	emissions         Emissions
}

var errSubBandNotFound = errors.DefineFailedPrecondition("sub_band_not_found", "sub-band not found for frequency `{frequency}` Hz")

func (s *Scheduler) findSubBand(frequency uint64) (*SubBand, error) {
	for _, subBand := range s.subBands {
		if subBand.Comprises(frequency) {
			return subBand, nil
		}
	}
	return nil, errSubBandNotFound.WithAttributes("frequency", frequency)
}

var (
	errDwellTime = errors.DefineFailedPrecondition("dwell_time", "packet exceeds dwell time restriction")
)

func (s *Scheduler) newEmission(payloadSize int, settings ttnpb.TxSettings) (Emission, error) {
	d, err := toa.Compute(payloadSize, settings)
	if err != nil {
		return Emission{}, err
	}
	if !s.respectsDwellTime(true, settings.Frequency, d) {
		return Emission{}, errDwellTime
	}
	var relative ConcentratorTime
	if settings.Time != nil {
		relative = s.clock.GatewayTime(*settings.Time)
	} else {
		relative = s.clock.TimestampTime(settings.Timestamp)
	}
	return NewEmission(relative, d), nil
}

var (
	errConflict = errors.DefineResourceExhausted("conflict", "scheduling conflict")
	errTooLate  = errors.DefineFailedPrecondition("too_late", "too late to transmission scheduled time (delta is `{delta}`)")
)

// ScheduleAt attempts to schedule the given Tx settings with the given priority.
func (s *Scheduler) ScheduleAt(ctx context.Context, payloadSize int, settings ttnpb.TxSettings, priority ttnpb.TxSchedulePriority) (Emission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clock.IsSynced() {
		now := s.clock.ServerTime(time.Now())
		if settings.Time != nil {
			if delta := time.Duration(s.clock.GatewayTime(*settings.Time) - now); delta < ScheduleTimeShort {
				return Emission{}, errTooLate.WithAttributes("delta", delta)
			}
		} else if delta := time.Duration(s.clock.TimestampTime(settings.Timestamp) - now); delta < ScheduleTimeShort {
			return Emission{}, errTooLate.WithAttributes("delta", delta)
		}
	}
	sb, err := s.findSubBand(settings.Frequency)
	if err != nil {
		return Emission{}, err
	}
	em, err := s.newEmission(payloadSize, settings)
	if err != nil {
		return Emission{}, err
	}
	for _, other := range s.emissions {
		if em.OverlapsWithOffAir(other, s.timeOffAir) {
			return Emission{}, errConflict
		}
	}
	if err := sb.Schedule(em, priority); err != nil {
		return Emission{}, err
	}
	s.emissions = s.emissions.Insert(em)
	return em, nil
}

// ScheduleAnytime attempts to schedule the given Tx settings with the given priority from the time in the settings.
// This method returns the emission.
//
// The scheduler does not support immediate scheduling, i.e. sending a message to the gateway that should be transmitted
// immediately. The reason for this is that this scheduler cannot determine conflicts or enforce duty-cycle when the
// emission time is unknown. Therefore, when the time is set to Immediate, the estimated current concentrator time plus
// ScheduleDelayLong will be used.
func (s *Scheduler) ScheduleAnytime(ctx context.Context, payloadSize int, settings ttnpb.TxSettings, priority ttnpb.TxSchedulePriority) (Emission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clock.IsSynced() {
		now := s.clock.ServerTime(time.Now())
		if settings.Timestamp == 0 && settings.Time == nil {
			target := time.Duration(now) + ScheduleTimeLong
			settings.Timestamp = uint32(target / time.Microsecond)
		} else if settings.Time != nil {
			if delta := time.Duration(s.clock.GatewayTime(*settings.Time) - now); delta < ScheduleTimeShort {
				t := settings.Time.Add(ScheduleTimeShort - delta)
				settings.Time = &t
			}
		} else if delta := time.Duration(s.clock.TimestampTime(settings.Timestamp) - now); delta < ScheduleTimeShort {
			settings.Timestamp += uint32((ScheduleTimeShort - delta) / time.Microsecond)
		}
	}
	sb, err := s.findSubBand(settings.Frequency)
	if err != nil {
		return Emission{}, err
	}
	em, err := s.newEmission(payloadSize, settings)
	if err != nil {
		return Emission{}, err
	}
	i := 0
	next := func() ConcentratorTime {
		if len(s.emissions) == 0 {
			// No emissions; schedule at the requested time.
			return em.t
		}
		for i < len(s.emissions)-1 {
			// Find a window between two emissions that does not conflict with either side.
			if em.OverlapsWithOffAir(s.emissions[i], s.timeOffAir) {
				// Schedule right after previous to resolve conflict.
				em.t = s.emissions[i].EndsWithOffAir(s.timeOffAir)
			}
			if em.OverlapsWithOffAir(s.emissions[i+1], s.timeOffAir) {
				// Schedule right after next to resolve conflict.
				em.t = s.emissions[i+1].EndsWithOffAir(s.timeOffAir)
				i++
				continue
			}
			// No conflicts, but advance counter for potential next iteration.
			// A next iteration can be necessary when this emission and priority exceeds a duty-cycle limitation.
			i++
			return em.t
		}
		// No emissions to schedule in between; schedule at timestamp or last transmission, whichever comes first.
		afterLast := s.emissions[len(s.emissions)-1].EndsWithOffAir(s.timeOffAir)
		if afterLast > em.t {
			return afterLast
		}
		return em.t
	}
	em, err = sb.ScheduleAnytime(em.d, next, priority)
	if err != nil {
		return Emission{}, err
	}
	s.emissions = s.emissions.Insert(em)
	return em, nil
}

// Sync synchronizes the clock with the given concentrator time v and the server time.
func (s *Scheduler) Sync(v uint32, server time.Time) {
	s.mu.Lock()
	s.clock.Sync(v, server)
	s.mu.Unlock()
}

// SyncWithGateway synchronizes the clock with the given concentrator time v, the server time and the gateway time that
// corresponds to the given v.
func (s *Scheduler) SyncWithGateway(v uint32, server, gateway time.Time) {
	s.mu.Lock()
	s.clock.SyncWithGateway(v, server, gateway)
	s.mu.Unlock()
}

// Now returns an indication of the current concentrator time.
// This method returns false if the clock is not synced with the server.
func (s *Scheduler) Now() (ConcentratorTime, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.clock.IsSynced() {
		return 0, false
	}
	return s.clock.ServerTime(time.Now()), true
}
