// Copyright (C) 2020 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mali

import (
	"context"
	"fmt"

	"github.com/google/gapid/core/log"
	"github.com/google/gapid/core/os/device"
	"github.com/google/gapid/gapis/api/sync"
	"github.com/google/gapid/gapis/perfetto"
	"github.com/google/gapid/gapis/service"
	"github.com/google/gapid/gapis/service/path"
	"github.com/google/gapid/gapis/trace/android/profile"
)

var (
	queueSubmitQuery = "" +
		"SELECT submission_id, command_buffer FROM gpu_slice s JOIN track t ON s.track_id = t.id WHERE s.name = 'vkQueueSubmit' AND t.name = 'Vulkan Events' ORDER BY submission_id"
	counterTracksQuery = "" +
		"SELECT id, name, unit, description FROM gpu_counter_track ORDER BY id"
	countersQueryFmt = "" +
		"SELECT ts, value FROM counter c WHERE c.track_id = %d ORDER BY ts"
)

func ProcessProfilingData(ctx context.Context, processor *perfetto.Processor, capture *path.Capture, desc *device.GpuCounterDescriptor, handleMapping map[uint64][]service.VulkanHandleMappingItem, syncData *sync.Data) (*service.ProfilingData, error) {
	slices, err := processGpuSlices(ctx, processor, capture, handleMapping, syncData)
	if err != nil {
		log.Err(ctx, err, "Failed to get GPU slices")
	}
	counters, err := processCounters(ctx, processor, desc)
	if err != nil {
		log.Err(ctx, err, "Failed to get GPU counters")
	}
	gpuCounters, err := profile.ComputeCounters(ctx, slices, counters)
	if err != nil {
		log.Err(ctx, err, "Failed to calculate performance data based on GPU slices and counters")
	}

	return &service.ProfilingData{
		Slices:      slices,
		Counters:    counters,
		GpuCounters: gpuCounters,
	}, nil
}

func processGpuSlices(ctx context.Context, processor *perfetto.Processor, capture *path.Capture, handleMapping map[uint64][]service.VulkanHandleMappingItem, syncData *sync.Data) (*service.ProfilingData_GpuSlices, error) {
	sliceData, err := profile.ExtractSliceData(ctx, processor)
	if err != nil {
		return nil, log.Errf(ctx, err, "Extracting slice data failed")
	}

	queueSubmitQueryResult, err := processor.Query(queueSubmitQuery)
	if err != nil {
		return nil, log.Errf(ctx, err, "SQL query failed: %v", queueSubmitQuery)
	}
	queueSubmitColumns := queueSubmitQueryResult.GetColumns()
	queueSubmitIds := queueSubmitColumns[0].GetLongValues()
	queueSubmitCommandBuffers := queueSubmitColumns[1].GetLongValues()
	submissionOrdering := make(map[int64]int)

	order := 0
	for i, v := range queueSubmitIds {
		if queueSubmitCommandBuffers[i] == 0 {
			// This is a spurious submission. See b/150854367
			log.W(ctx, "Spurious vkQueueSubmit slice with submission id %v", v)
			continue
		}
		submissionOrdering[v] = order
		order++
	}

	sliceData.MapIdentifiers(ctx, handleMapping)

	groupId := int32(-1)
	for i, v := range sliceData.Submissions {
		subOrder, ok := submissionOrdering[v]
		if ok {
			cb := uint64(sliceData.CommandBuffers[i])
			key := sync.RenderPassKey{
				subOrder, cb, uint64(sliceData.RenderPasses[i]), uint64(sliceData.RenderTargets[i]),
			}
			// Create a new group for each main renderPass slice.
			name := sliceData.Names[i]
			indices := syncData.RenderPassLookup.Lookup(ctx, key)
			if !indices.IsNil() && (name == "vertex" || name == "fragment") {
				sliceData.Names[i] = fmt.Sprintf("%v-%v %v", indices.From, indices.To, name)
				groupId = sliceData.CreateOrGetGroup(
					fmt.Sprintf("RenderPass %v, RenderTarget %v", uint64(sliceData.RenderPasses[i]), uint64(sliceData.RenderTargets[i])),
					indices,
				)
			}
		} else {
			log.W(ctx, "Encountered submission ID mismatch %v", v)
		}

		if groupId < 0 {
			log.W(ctx, "Group missing for slice %v at submission %v, commandBuffer %v, renderPass %v, renderTarget %v",
				sliceData.Names[i], sliceData.Submissions[i], sliceData.CommandBuffers[i], sliceData.RenderPasses[i], sliceData.RenderTargets[i])
		}
		sliceData.GroupIds[i] = groupId
	}

	return sliceData.ToService(ctx, processor, capture), nil
}

func processCounters(ctx context.Context, processor *perfetto.Processor, desc *device.GpuCounterDescriptor) ([]*service.ProfilingData_Counter, error) {
	counterTracksQueryResult, err := processor.Query(counterTracksQuery)
	if err != nil {
		return nil, log.Errf(ctx, err, "SQL query failed: %v", counterTracksQuery)
	}
	// t.id, name, unit, description, ts, value
	tracksColumns := counterTracksQueryResult.GetColumns()
	numTracksRows := counterTracksQueryResult.GetNumRecords()
	counters := make([]*service.ProfilingData_Counter, numTracksRows)
	// Grab all the column values. Depends on the order of columns selected in countersQuery
	trackIds := tracksColumns[0].GetLongValues()
	names := tracksColumns[1].GetStringValues()
	units := tracksColumns[2].GetStringValues()
	descriptions := tracksColumns[3].GetStringValues()

	nameToSpec := map[string]*device.GpuCounterDescriptor_GpuCounterSpec{}
	if desc != nil {
		for _, spec := range desc.Specs {
			nameToSpec[spec.Name] = spec
		}
	}

	for i := uint64(0); i < numTracksRows; i++ {
		countersQuery := fmt.Sprintf(countersQueryFmt, trackIds[i])
		countersQueryResult, err := processor.Query(countersQuery)
		countersColumns := countersQueryResult.GetColumns()
		if err != nil {
			return nil, log.Errf(ctx, err, "SQL query failed: %v", counterTracksQuery)
		}
		timestampsLong := countersColumns[0].GetLongValues()
		timestamps := make([]uint64, len(timestampsLong))
		for i, t := range timestampsLong {
			timestamps[i] = uint64(t)
		}
		values := countersColumns[1].GetDoubleValues()

		spec, _ := nameToSpec[names[i]]
		// TODO(apbodnar) Populate the `default` field once the trace processor supports it (b/147432390)
		counters[i] = &service.ProfilingData_Counter{
			Id:          uint32(trackIds[i]),
			Name:        names[i],
			Unit:        units[i],
			Description: descriptions[i],
			Spec:        spec,
			Timestamps:  timestamps,
			Values:      values,
		}
	}
	return counters, nil
}
