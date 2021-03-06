/*
Copyright 2017, 2019 the Velero contributors.

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

package plugin

import (
	"encoding/json"

	plugin "github.com/hashicorp/go-plugin"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	api "github.com/heptio/velero/pkg/apis/velero/v1"
	proto "github.com/heptio/velero/pkg/plugin/generated"
	"github.com/heptio/velero/pkg/plugin/velero"
)

// RestoreItemActionPlugin is an implementation of go-plugin's Plugin
// interface with support for gRPC for the restore/ItemAction
// interface.
type RestoreItemActionPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	*pluginBase
}

var _ velero.RestoreItemAction = &RestoreItemActionGRPCClient{}

// NewRestoreItemActionPlugin constructs a RestoreItemActionPlugin.
func NewRestoreItemActionPlugin(options ...pluginOption) *RestoreItemActionPlugin {
	return &RestoreItemActionPlugin{
		pluginBase: newPluginBase(options...),
	}
}

//////////////////////////////////////////////////////////////////////////////
// client code
//////////////////////////////////////////////////////////////////////////////

// GRPCClient returns a RestoreItemAction gRPC client.
func (p *RestoreItemActionPlugin) GRPCClient(c *grpc.ClientConn) (interface{}, error) {
	return newClientDispenser(p.clientLogger, c, newRestoreItemActionGRPCClient), nil
}

// RestoreItemActionGRPCClient implements the backup/ItemAction interface and uses a
// gRPC client to make calls to the plugin server.
type RestoreItemActionGRPCClient struct {
	*clientBase
	grpcClient proto.RestoreItemActionClient
}

func newRestoreItemActionGRPCClient(base *clientBase, clientConn *grpc.ClientConn) interface{} {
	return &RestoreItemActionGRPCClient{
		clientBase: base,
		grpcClient: proto.NewRestoreItemActionClient(clientConn),
	}
}

func (c *RestoreItemActionGRPCClient) AppliesTo() (velero.ResourceSelector, error) {
	res, err := c.grpcClient.AppliesTo(context.Background(), &proto.AppliesToRequest{Plugin: c.plugin})
	if err != nil {
		return velero.ResourceSelector{}, err
	}

	return velero.ResourceSelector{
		IncludedNamespaces: res.IncludedNamespaces,
		ExcludedNamespaces: res.ExcludedNamespaces,
		IncludedResources:  res.IncludedResources,
		ExcludedResources:  res.ExcludedResources,
		LabelSelector:      res.Selector,
	}, nil
}

func (c *RestoreItemActionGRPCClient) Execute(input *velero.RestoreItemActionExecuteInput) (*velero.RestoreItemActionExecuteOutput, error) {
	itemJSON, err := json.Marshal(input.Item.UnstructuredContent())
	if err != nil {
		return nil, err
	}

	itemFromBackupJSON, err := json.Marshal(input.ItemFromBackup.UnstructuredContent())
	if err != nil {
		return nil, err
	}

	restoreJSON, err := json.Marshal(input.Restore)
	if err != nil {
		return nil, err
	}

	req := &proto.RestoreExecuteRequest{
		Plugin:         c.plugin,
		Item:           itemJSON,
		ItemFromBackup: itemFromBackupJSON,
		Restore:        restoreJSON,
	}

	res, err := c.grpcClient.Execute(context.Background(), req)
	if err != nil {
		return nil, err
	}

	var updatedItem unstructured.Unstructured
	if err := json.Unmarshal(res.Item, &updatedItem); err != nil {
		return nil, err
	}

	var warning error
	if res.Warning != "" {
		warning = errors.New(res.Warning)
	}

	return &velero.RestoreItemActionExecuteOutput{
		UpdatedItem: &updatedItem,
		Warning:     warning,
	}, nil
}

//////////////////////////////////////////////////////////////////////////////
// server code
//////////////////////////////////////////////////////////////////////////////

// GRPCServer registers a RestoreItemAction gRPC server.
func (p *RestoreItemActionPlugin) GRPCServer(s *grpc.Server) error {
	proto.RegisterRestoreItemActionServer(s, &RestoreItemActionGRPCServer{mux: p.serverMux})
	return nil
}

// RestoreItemActionGRPCServer implements the proto-generated RestoreItemActionServer interface, and accepts
// gRPC calls and forwards them to an implementation of the pluggable interface.
type RestoreItemActionGRPCServer struct {
	mux *serverMux
}

func (s *RestoreItemActionGRPCServer) getImpl(name string) (velero.RestoreItemAction, error) {
	impl, err := s.mux.getHandler(name)
	if err != nil {
		return nil, err
	}

	itemAction, ok := impl.(velero.RestoreItemAction)
	if !ok {
		return nil, errors.Errorf("%T is not a restore item action", impl)
	}

	return itemAction, nil
}

func (s *RestoreItemActionGRPCServer) AppliesTo(ctx context.Context, req *proto.AppliesToRequest) (response *proto.AppliesToResponse, err error) {
	defer func() {
		if recoveredErr := handlePanic(recover()); recoveredErr != nil {
			err = recoveredErr
		}
	}()

	impl, err := s.getImpl(req.Plugin)
	if err != nil {
		return nil, err
	}

	appliesTo, err := impl.AppliesTo()
	if err != nil {
		return nil, err
	}

	return &proto.AppliesToResponse{
		IncludedNamespaces: appliesTo.IncludedNamespaces,
		ExcludedNamespaces: appliesTo.ExcludedNamespaces,
		IncludedResources:  appliesTo.IncludedResources,
		ExcludedResources:  appliesTo.ExcludedResources,
		Selector:           appliesTo.LabelSelector,
	}, nil
}

func (s *RestoreItemActionGRPCServer) Execute(ctx context.Context, req *proto.RestoreExecuteRequest) (response *proto.RestoreExecuteResponse, err error) {
	defer func() {
		if recoveredErr := handlePanic(recover()); recoveredErr != nil {
			err = recoveredErr
		}
	}()

	impl, err := s.getImpl(req.Plugin)
	if err != nil {
		return nil, err
	}

	var (
		item           unstructured.Unstructured
		itemFromBackup unstructured.Unstructured
		restoreObj     api.Restore
	)

	if err := json.Unmarshal(req.Item, &item); err != nil {
		return nil, err
	}

	if err := json.Unmarshal(req.ItemFromBackup, &itemFromBackup); err != nil {
		return nil, err
	}

	if err := json.Unmarshal(req.Restore, &restoreObj); err != nil {
		return nil, err
	}

	executeOutput, err := impl.Execute(&velero.RestoreItemActionExecuteInput{
		Item:           &item,
		ItemFromBackup: &itemFromBackup,
		Restore:        &restoreObj,
	})
	if err != nil {
		return nil, err
	}

	updatedItem, err := json.Marshal(executeOutput.UpdatedItem)
	if err != nil {
		return nil, err
	}

	var warnMessage string
	if executeOutput.Warning != nil {
		warnMessage = executeOutput.Warning.Error()
	}

	return &proto.RestoreExecuteResponse{
		Item:    updatedItem,
		Warning: warnMessage,
	}, nil
}
