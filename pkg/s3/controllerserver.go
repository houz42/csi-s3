/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package s3

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
	csicommon "github.com/kubernetes-csi/drivers/pkg/csi-common"
)

type controllerServer struct {
	*csicommon.DefaultControllerServer
}

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	volumeID := sanitizeVolumeID(req.GetName())

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid create volume req: %v", req)
		return nil, err
	}

	// Check arguments
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities missing in request")
	}

	capacityBytes := int64(req.GetCapacityRange().GetRequiredBytes())
	params := req.GetParameters()

	volume := &volume{
		ID:       volumeID,
		Bucket:   params[volumeAttributeBucket],
		Prefix:   params[volumeAttributePrefix],
		Capacity: int64(req.GetCapacityRange().GetRequiredBytes()),
	}

	glog.V(4).Infof("Got a request to create volume  %v", volume)

	s3, err := newS3ClientFromSecrets(req.GetSecrets())
	if err != nil {
		return nil, fmt.Errorf("failed to initialize S3 client: %w", err)
	}
	s3.completeVolume(volume)
	params[volumeAttributeBucket] = volume.Bucket
	params[volumeAttributePrefix] = volume.Prefix

	exists, err := s3.volumeExists(volume)
	if err != nil || !exists {
		if err := s3.createVolume(volume); err != nil {
			return nil, fmt.Errorf("failed to initialize empty volume:%v: %w", volume, err)
		}
	} else {
		if e := s3.getVolume(volume); e != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("get volume metadata: %v", e))
		}
		// Check if volume capacity requested is bigger than the already existing capacity
		if req.GetCapacityRange().GetRequiredBytes() > volume.Capacity {
			return nil, status.Error(codes.AlreadyExists, fmt.Sprintf("volume with the same name (%s) but with smaller size already exists", volume.ID))
		}
	}

	glog.V(4).Infof("create volume %s", volumeID)

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: capacityBytes,
			VolumeContext: params,
		},
	}, nil
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()

	// Check arguments
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("Invalid delete volume req: %v", req)
		return nil, err
	}
	glog.V(4).Infof("Deleting volume %s", volumeID)

	s3, err := newS3ClientFromSecrets(req.GetSecrets())
	if err != nil {
		return nil, fmt.Errorf("failed to initialize S3 client: %s", err)
	}
	exists, err := s3.volumeExists(&volume{ID: volumeID})
	if err != nil || !exists {
		glog.V(3).Infof("volume does not exist, return without error, internal error: %v", err)
		return &csi.DeleteVolumeResponse{}, nil
	}
	if err := s3.removeVolume(&volume{ID: volumeID}); err != nil {
		glog.V(3).Infof("Failed to remove volume %s: %v", volumeID, err)
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to delete volume %s, error: %v", volumeID, err))
	}

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing in request")
	}

	s3, err := newS3ClientFromSecrets(req.GetSecrets())
	if err != nil {
		return nil, fmt.Errorf("failed to initialize S3 client: %s", err)
	}
	exists, err := s3.volumeExists(&volume{ID: req.GetVolumeId()})
	if err != nil || !exists {
		// return an error if the volume requested does not exist
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume with id %s does not exist", req.GetVolumeId()))
	}

	// We currently only support RWO
	supportedAccessMode := &csi.VolumeCapability_AccessMode{
		Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	}

	for _, cap := range req.VolumeCapabilities {
		if cap.GetAccessMode().GetMode() != supportedAccessMode.GetMode() {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: "Only single node writer is supported"}, nil
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: []*csi.VolumeCapability{
				{
					AccessMode: supportedAccessMode,
				},
			},
		},
	}, nil
}

func (cs *controllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return &csi.ControllerExpandVolumeResponse{}, status.Error(codes.Unimplemented, "ControllerExpandVolume is not implemented")
}

func sanitizeVolumeID(volumeID string) string {
	volumeID = strings.ToLower(volumeID)
	if len(volumeID) > 63 {
		h := sha1.New()
		io.WriteString(h, volumeID)
		volumeID = hex.EncodeToString(h.Sum(nil))
	}
	return volumeID
}
