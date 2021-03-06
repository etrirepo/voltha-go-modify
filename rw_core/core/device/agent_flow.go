/*
 * Copyright 2018-present Open Networking Foundation

 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at

 * http://www.apache.org/licenses/LICENSE-2.0

 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package device

import (
	"context"

	"github.com/gogo/protobuf/proto"
	coreutils "github.com/opencord/voltha-go/rw_core/utils"
	fu "github.com/opencord/voltha-lib-go/v4/pkg/flows"
	"github.com/opencord/voltha-lib-go/v4/pkg/log"
	ofp "github.com/opencord/voltha-protos/v4/go/openflow_13"
	"github.com/opencord/voltha-protos/v4/go/voltha"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// listDeviceFlows returns device flows
func (agent *Agent) listDeviceFlows() map[uint64]*ofp.OfpFlowStats {
	flowIDs := agent.flowLoader.ListIDs()
	agent.customLoader.ListIDs()
	flows := make(map[uint64]*ofp.OfpFlowStats, len(flowIDs))
	for flowID := range flowIDs {
		if flowHandle, have := agent.flowLoader.Lock(flowID); have {
			flows[flowID] = flowHandle.GetReadOnly()
			flowHandle.Unlock()
		}
	}
	return flows
}

func (agent *Agent) addFlowsToAdapter(ctx context.Context, newFlows []*ofp.OfpFlowStats, flowMetadata *voltha.FlowMetadata) (coreutils.Response, error) {
	logger.Debugw(ctx, "add-flows-to-adapters", log.Fields{"device-id": agent.deviceID, "flows": newFlows, "flow-metadata": flowMetadata})

	if (len(newFlows)) == 0 {
		logger.Debugw(ctx, "nothing-to-update", log.Fields{"device-id": agent.deviceID, "flows": newFlows})
		return coreutils.DoneResponse(), nil
	}
	device, err := agent.getDeviceReadOnly(ctx)
	if err != nil {
		return coreutils.DoneResponse(), status.Errorf(codes.Aborted, "%s", err)
	}
	dType, err := agent.adapterMgr.GetDeviceType(ctx, &voltha.ID{Id: device.Type})
	if err != nil {
		return coreutils.DoneResponse(), status.Errorf(codes.FailedPrecondition, "non-existent-device-type-%s", device.Type)
	}
	flowsToAdd := make([]*ofp.OfpFlowStats, 0)
	flowsToDelete := make([]*ofp.OfpFlowStats, 0)
	for _, flow := range newFlows {
		flowHandle, created, err := agent.flowLoader.LockOrCreate(ctx, flow)
		flowHandle2, created2, err2:= agent.customLoader.LockOrCreate(ctx, flow)
		if err != nil {
			return coreutils.DoneResponse(), err
		}
		if err2 != nil {
			return coreutils.DoneResponse(), err
		}
		if created {
			flowsToAdd = append(flowsToAdd, flow)
		} else {
			flowToReplace := flowHandle.GetReadOnly()
			if !proto.Equal(flowToReplace, flow) {
				//Flow needs to be updated.
				if err := flowHandle.Update(ctx, flow); err != nil {
					flowHandle.Unlock()
					return coreutils.DoneResponse(), status.Errorf(codes.Internal, "failure-updating-flow-%d-to-device-%s", flow.Id, agent.deviceID)
				}
				flowsToDelete = append(flowsToDelete, flowToReplace)
				flowsToAdd = append(flowsToAdd, flow)
			} else {
				//No need to change the flow. It is already exist.
				logger.Debugw(ctx, "No-need-to-change-already-existing-flow", log.Fields{"device-id": agent.deviceID, "flows": newFlows, "flow-metadata": flowMetadata})
			}
		}
		flowHandle.Unlock()
		if created2 {
		} else {
			flowToReplace := flowHandle2.GetReadOnly()
			if !proto.Equal(flowToReplace, flow) {
				//Flow needs to be updated.
				if err := flowHandle2.Update(ctx, flow); err != nil {
					flowHandle2.Unlock()
					return coreutils.DoneResponse(), status.Errorf(codes.Internal, "20210722 Yundaehee-failure-updating-flow-%d-to-device-%s", flow.Id, agent.deviceID)
				}
			} else {
				//No need to change the flow. It is already exist.
				logger.Debugw(ctx, "20210722 Yundaehe eNo-need-to-change-already-existing-flow", log.Fields{"device-id": agent.deviceID, "flows": newFlows, "flow-metadata": flowMetadata})
			}
		}
		flowHandle2.Unlock()

	}

	// Sanity check
	if (len(flowsToAdd)) == 0 {
		logger.Debugw(ctx, "no-flows-to-update", log.Fields{"device-id": agent.deviceID, "flows": newFlows})
		return coreutils.DoneResponse(), nil
	}

	// Send update to adapters
	subCtx, cancel := context.WithTimeout(log.WithSpanFromContext(context.Background(), ctx), agent.defaultTimeout)
	response := coreutils.NewResponse()
	if !dType.AcceptsAddRemoveFlowUpdates {

		updatedAllFlows := agent.listDeviceFlows()
		rpcResponse, err := agent.adapterProxy.UpdateFlowsBulk(subCtx, device, updatedAllFlows, nil, flowMetadata)
		if err != nil {
			cancel()
			return coreutils.DoneResponse(), err
		}
		go agent.waitForAdapterFlowResponse(subCtx, cancel, rpcResponse, response)
	} else {
		flowChanges := &ofp.FlowChanges{
			ToAdd:    &voltha.Flows{Items: flowsToAdd},
			ToRemove: &voltha.Flows{Items: flowsToDelete},
		}
		groupChanges := &ofp.FlowGroupChanges{
			ToAdd:    &voltha.FlowGroups{Items: []*ofp.OfpGroupEntry{}},
			ToRemove: &voltha.FlowGroups{Items: []*ofp.OfpGroupEntry{}},
			ToUpdate: &voltha.FlowGroups{Items: []*ofp.OfpGroupEntry{}},
		}
		rpcResponse, err := agent.adapterProxy.UpdateFlowsIncremental(subCtx, device, flowChanges, groupChanges, flowMetadata)
		if err != nil {
			cancel()
			return coreutils.DoneResponse(), err
		}
		go agent.waitForAdapterFlowResponse(subCtx, cancel, rpcResponse, response)
	}
	return response, nil
}

func (agent *Agent) deleteFlowsFromAdapter(ctx context.Context, flowsToDel []*ofp.OfpFlowStats, flowMetadata *voltha.FlowMetadata) (coreutils.Response, error) {
	logger.Debugw(ctx, "delete-flows-from-adapter", log.Fields{"device-id": agent.deviceID, "flows": flowsToDel})

	if (len(flowsToDel)) == 0 {
		logger.Debugw(ctx, "nothing-to-delete", log.Fields{"device-id": agent.deviceID, "flows": flowsToDel})
		return coreutils.DoneResponse(), nil
	}

	device, err := agent.getDeviceReadOnly(ctx)
	if err != nil {
		return coreutils.DoneResponse(), status.Errorf(codes.Aborted, "%s", err)
	}
	dType, err := agent.adapterMgr.GetDeviceType(ctx, &voltha.ID{Id: device.Type})
	if err != nil {
		return coreutils.DoneResponse(), status.Errorf(codes.FailedPrecondition, "non-existent-device-type-%s", device.Type)
	}
	for _, flow := range flowsToDel {
		if flowHandle, have := agent.flowLoader.Lock(flow.Id); have {
			// Update the store and cache
			if err := flowHandle.Delete(ctx); err != nil {
				flowHandle.Unlock()
				return coreutils.DoneResponse(), err
			}
			flowHandle.Unlock()
		}
	}

	// Send update to adapters
	subCtx, cancel := context.WithTimeout(log.WithSpanFromContext(context.Background(), ctx), agent.defaultTimeout)
	response := coreutils.NewResponse()
	if !dType.AcceptsAddRemoveFlowUpdates {

		updatedAllFlows := agent.listDeviceFlows()
		rpcResponse, err := agent.adapterProxy.UpdateFlowsBulk(subCtx, device, updatedAllFlows, nil, flowMetadata)
		if err != nil {
			cancel()
			return coreutils.DoneResponse(), err
		}
		go agent.waitForAdapterFlowResponse(subCtx, cancel, rpcResponse, response)
	} else {
		flowChanges := &ofp.FlowChanges{
			ToAdd:    &voltha.Flows{Items: []*ofp.OfpFlowStats{}},
			ToRemove: &voltha.Flows{Items: flowsToDel},
		}
		groupChanges := &ofp.FlowGroupChanges{
			ToAdd:    &voltha.FlowGroups{Items: []*ofp.OfpGroupEntry{}},
			ToRemove: &voltha.FlowGroups{Items: []*ofp.OfpGroupEntry{}},
			ToUpdate: &voltha.FlowGroups{Items: []*ofp.OfpGroupEntry{}},
		}
		rpcResponse, err := agent.adapterProxy.UpdateFlowsIncremental(subCtx, device, flowChanges, groupChanges, flowMetadata)
		if err != nil {
			cancel()
			return coreutils.DoneResponse(), err
		}
		go agent.waitForAdapterFlowResponse(subCtx, cancel, rpcResponse, response)
	}
	return response, nil
}

func (agent *Agent) updateFlowsToAdapter(ctx context.Context, updatedFlows []*ofp.OfpFlowStats, flowMetadata *voltha.FlowMetadata) (coreutils.Response, error) {
	logger.Debugw(ctx, "updateFlowsToAdapter", log.Fields{"device-id": agent.deviceID, "flows": updatedFlows})

	if (len(updatedFlows)) == 0 {
		logger.Debugw(ctx, "nothing-to-update", log.Fields{"device-id": agent.deviceID, "flows": updatedFlows})
		return coreutils.DoneResponse(), nil
	}

	device, err := agent.getDeviceReadOnly(ctx)
	if err != nil {
		return coreutils.DoneResponse(), status.Errorf(codes.Aborted, "%s", err)
	}
	if device.OperStatus != voltha.OperStatus_ACTIVE || device.ConnectStatus != voltha.ConnectStatus_REACHABLE || device.AdminState != voltha.AdminState_ENABLED {
		return coreutils.DoneResponse(), status.Errorf(codes.FailedPrecondition, "invalid device states")
	}
	dType, err := agent.adapterMgr.GetDeviceType(ctx, &voltha.ID{Id: device.Type})
	if err != nil {
		return coreutils.DoneResponse(), status.Errorf(codes.FailedPrecondition, "non-existent-device-type-%s", device.Type)
	}
	flowsToAdd := make([]*ofp.OfpFlowStats, 0, len(updatedFlows))
	flowsToDelete := make([]*ofp.OfpFlowStats, 0, len(updatedFlows))
	for _, flow := range updatedFlows {
		if flowHandle, have := agent.flowLoader.Lock(flow.Id); have {
			flowToDelete := flowHandle.GetReadOnly()
			// Update the store and cache
			if err := flowHandle.Update(ctx, flow); err != nil {
				flowHandle.Unlock()
				return coreutils.DoneResponse(), err
			}

			flowsToDelete = append(flowsToDelete, flowToDelete)
			flowsToAdd = append(flowsToAdd, flow)
			flowHandle.Unlock()
		}
	}

	subCtx, cancel := context.WithTimeout(log.WithSpanFromContext(context.Background(), ctx), agent.defaultTimeout)
	response := coreutils.NewResponse()
	// Process bulk flow update differently than incremental update
	if !dType.AcceptsAddRemoveFlowUpdates {
		updatedAllFlows := agent.listDeviceFlows()
		rpcResponse, err := agent.adapterProxy.UpdateFlowsBulk(subCtx, device, updatedAllFlows, nil, nil)
		if err != nil {
			cancel()
			return coreutils.DoneResponse(), err
		}
		go agent.waitForAdapterFlowResponse(subCtx, cancel, rpcResponse, response)
	} else {
		logger.Debugw(ctx, "updating-flows-and-groups",
			log.Fields{
				"device-id":       agent.deviceID,
				"flows-to-add":    flowsToAdd,
				"flows-to-delete": flowsToDelete,
			})
		// Sanity check
		if (len(flowsToAdd) | len(flowsToDelete)) == 0 {
			logger.Debugw(ctx, "nothing-to-update", log.Fields{"device-id": agent.deviceID, "flows": updatedFlows})
			cancel()
			return coreutils.DoneResponse(), nil
		}

		flowChanges := &ofp.FlowChanges{
			ToAdd:    &voltha.Flows{Items: flowsToAdd},
			ToRemove: &voltha.Flows{Items: flowsToDelete},
		}
		groupChanges := &ofp.FlowGroupChanges{
			ToAdd:    &voltha.FlowGroups{Items: []*ofp.OfpGroupEntry{}},
			ToRemove: &voltha.FlowGroups{Items: []*ofp.OfpGroupEntry{}},
			ToUpdate: &voltha.FlowGroups{Items: []*ofp.OfpGroupEntry{}},
		}
		rpcResponse, err := agent.adapterProxy.UpdateFlowsIncremental(subCtx, device, flowChanges, groupChanges, flowMetadata)
		if err != nil {
			cancel()
			return coreutils.DoneResponse(), err
		}
		go agent.waitForAdapterFlowResponse(subCtx, cancel, rpcResponse, response)
	}

	return response, nil
}

//filterOutFlows removes flows from a device using the uni-port as filter
func (agent *Agent) filterOutFlows(ctx context.Context, uniPort uint32, flowMetadata *voltha.FlowMetadata) error {
	var flowsToDelete []*ofp.OfpFlowStats
	// If an existing flow has the uniPort as an InPort or OutPort or as a Tunnel ID then it needs to be removed
	for flowID := range agent.flowLoader.ListIDs() {
		if flowHandle, have := agent.flowLoader.Lock(flowID); have {
			flow := flowHandle.GetReadOnly()
			if flow != nil && (fu.GetInPort(flow) == uniPort || fu.GetOutPort(flow) == uniPort || fu.GetTunnelId(flow) == uint64(uniPort)) {
				flowsToDelete = append(flowsToDelete, flow)
			}
			flowHandle.Unlock()
		}
	}

	logger.Debugw(ctx, "flows-to-delete", log.Fields{"device-id": agent.deviceID, "uni-port": uniPort, "flows": flowsToDelete})
	if len(flowsToDelete) == 0 {
		return nil
	}

	response, err := agent.deleteFlowsFromAdapter(ctx, flowsToDelete, flowMetadata)
	if err != nil {
		return err
	}
	if res := coreutils.WaitForNilOrErrorResponses(agent.defaultTimeout, response); res != nil {
		return status.Errorf(codes.Aborted, "errors-%s", res)
	}
	return nil
}

//deleteAllFlows deletes all flows in the device table
func (agent *Agent) deleteAllFlows(ctx context.Context) error {
	logger.Debugw(ctx, "deleteAllFlows", log.Fields{"device-id": agent.deviceID})

	for flowID := range agent.flowLoader.ListIDs() {
		if flowHandle, have := agent.flowLoader.Lock(flowID); have {
			// Update the store and cache
			if err := flowHandle.Delete(ctx); err != nil {
				flowHandle.Unlock()
				logger.Errorw(ctx, "unable-to-delete-flow", log.Fields{"device-id": agent.deviceID, "flowID": flowID})
				continue
			}
			flowHandle.Unlock()
		}
	}
	return nil
}
