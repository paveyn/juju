// Copyright 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package azure

import (
	"fmt"
	"sync"

	"launchpad.net/gwacl"
	"launchpad.net/juju-core/constraints"
	"launchpad.net/juju-core/environs"
	"launchpad.net/juju-core/environs/config"
	"launchpad.net/juju-core/instance"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/state/api"
)

type azureEnviron struct {
	// Except where indicated otherwise, all fields in this object should
	// only be accessed using a lock or a snapshot.
	sync.Mutex

	// name is immutable; it does not need locking.
	name string

	// ecfg is the environment's Azure-specific configuration.
	ecfg *azureEnvironConfig

	// storage is this environ's own private storage.
	storage environs.Storage

	// publicStorage is the public storage that this environ uses.
	publicStorage environs.StorageReader
}

// azureEnviron implements Environ.
var _ environs.Environ = (*azureEnviron)(nil)

// NewEnviron creates a new azureEnviron.
func NewEnviron(cfg *config.Config) (*azureEnviron, error) {
	env := azureEnviron{name: cfg.Name()}
	err := env.SetConfig(cfg)
	if err != nil {
		return nil, err
	}

	// Set up storage.
	env.storage = &azureStorage{
		storageContext: &environStorageContext{environ: &env},
	}

	// Set up public storage.
	publicContext := publicEnvironStorageContext{environ: &env}
	if publicContext.getContainer() == "" {
		// No public storage configured.  Use EmptyStorage.
		env.publicStorage = environs.EmptyStorage
	} else {
		// Set up real public storage.
		env.publicStorage = &azureStorage{storageContext: &publicContext}
	}

	return &env, nil
}

// Name is specified in the Environ interface.
func (env *azureEnviron) Name() string {
	return env.name
}

// getSnapshot produces an atomic shallow copy of the environment object.
// Whenever you need to access the environment object's fields without
// modifying them, get a snapshot and read its fields instead.  You will
// get a consistent view of the fields without any further locking.
// If you do need to modify the environment's fields, do not get a snapshot
// but lock the object throughout the critical section.
func (env *azureEnviron) getSnapshot() *azureEnviron {
	env.Lock()
	defer env.Unlock()

	// Copy the environment.  (Not the pointer, the environment itself.)
	// This is a shallow copy.
	snap := *env
	// Reset the snapshot's mutex, because we just copied it while we
	// were holding it.  The snapshot will have a "clean," unlocked mutex.
	snap.Mutex = sync.Mutex{}
	return &snap
}

// Bootstrap is specified in the Environ interface.
func (env *azureEnviron) Bootstrap(cons constraints.Value) error {
	panic("unimplemented")
}

// StateInfo is specified in the Environ interface.
func (env *azureEnviron) StateInfo() (*state.Info, *api.Info, error) {
	return environs.StateInfo(env)
}

// Config is specified in the Environ interface.
func (env *azureEnviron) Config() *config.Config {
	snap := env.getSnapshot()
	return snap.ecfg.Config
}

// SetConfig is specified in the Environ interface.
func (env *azureEnviron) SetConfig(cfg *config.Config) error {
	ecfg, err := azureEnvironProvider{}.newConfig(cfg)
	if err != nil {
		return err
	}

	env.Lock()
	defer env.Unlock()

	if env.ecfg != nil {
		_, err = azureEnvironProvider{}.Validate(cfg, env.ecfg.Config)
		if err != nil {
			return err
		}
	}

	env.ecfg = ecfg
	return nil
}

// StartInstance is specified in the Environ interface.
func (env *azureEnviron) StartInstance(machineId, machineNonce string, series string, cons constraints.Value,
	info *state.Info, apiInfo *api.Info) (instance.Instance, *instance.HardwareCharacteristics, error) {
	panic("unimplemented")
}

// StopInstances is specified in the Environ interface.
func (env *azureEnviron) StopInstances(instances []instance.Instance) error {
	// Shortcut to exit quickly if 'instances' is an empty slice or nil.
	if len(instances) == 0 {
		return nil
	}
	// Acquire management API object.
	context, err := env.getManagementAPI()
	if err != nil {
		return err
	}
	defer env.releaseManagementAPI(context)
	// Shut down all the instances; if there are errors, return only the
	// first one (but try to shut down all instances regardless).
	var firstErr error
	for _, instance := range instances {
		request := &gwacl.DestroyHostedServiceRequest{ServiceName: string(instance.Id())}
		err := context.DestroyHostedService(request)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Instances is specified in the Environ interface.
func (env *azureEnviron) Instances(ids []instance.Id) ([]instance.Instance, error) {
	// The instance list is built using the list of all the relevant
	// Azure Services (instance==service).
	// Acquire management API object.
	context, err := env.getManagementAPI()
	if err != nil {
		return nil, err
	}
	defer env.releaseManagementAPI(context)

	// Prepare gwacl request object.
	serviceNames := make([]string, len(ids))
	for i, id := range ids {
		serviceNames[i] = string(id)
	}
	request := &gwacl.ListSpecificHostedServicesRequest{ServiceNames: serviceNames}

	// Issue 'ListSpecificHostedServices' request with gwacl.
	services, err := context.ListSpecificHostedServices(request)
	if err != nil {
		return nil, err
	}

	// If no instances were found, return ErrNoInstances.
	if len(services) == 0 {
		return nil, environs.ErrNoInstances
	}

	instances := convertToInstances(services)

	// Check if we got a partial result.
	if len(ids) != len(instances) {
		return instances, environs.ErrPartialInstances
	}
	return instances, nil
}

// AllInstances is specified in the Environ interface.
func (env *azureEnviron) AllInstances() ([]instance.Instance, error) {
	// The instance list is built using the list of all the Azure
	// Services (instance==service).
	// Acquire management API object.
	context, err := env.getManagementAPI()
	if err != nil {
		return nil, err
	}
	defer env.releaseManagementAPI(context)

	request := &gwacl.ListPrefixedHostedServicesRequest{ServiceNamePrefix: env.getEnvPrefix()}
	services, err := context.ListPrefixedHostedServices(request)
	if err != nil {
		return nil, err
	}
	return convertToInstances(services), nil
}

// getEnvPrefix returns the prefix used to name the objects specific to this
// environment.
func (env *azureEnviron) getEnvPrefix() string {
	return fmt.Sprintf("juju-%s", env.Name())
}

// convertToInstances converts a slice of gwacl.HostedServiceDescriptor objects
// into a slice of instance.Instance objects.
func convertToInstances(services []gwacl.HostedServiceDescriptor) []instance.Instance {
	instances := make([]instance.Instance, len(services))
	for i, service := range services {
		instances[i] = &azureInstance{service}
	}
	return instances
}

// Storage is specified in the Environ interface.
func (env *azureEnviron) Storage() environs.Storage {
	return env.getSnapshot().storage
}

// PublicStorage is specified in the Environ interface.
func (env *azureEnviron) PublicStorage() environs.StorageReader {
	return env.getSnapshot().publicStorage
}

// Destroy is specified in the Environ interface.
func (env *azureEnviron) Destroy(ensureInsts []instance.Instance) error {
	logger.Debugf("destroying environment %q", env.name)

	// Delete storage.
	st := env.Storage().(*azureStorage)
	context, err := st.getStorageContext()
	if err != nil {
		return err
	}
	request := &gwacl.DeleteAllBlobsRequest{Container: st.getContainer()}
	err = context.DeleteAllBlobs(request)
	if err != nil {
		return fmt.Errorf("cannot clean up storage: %v", err)
	}

	// Stop all instances.
	insts, err := env.AllInstances()
	if err != nil {
		return fmt.Errorf("cannot get instances: %v", err)
	}
	found := make(map[instance.Id]bool)
	for _, inst := range insts {
		found[inst.Id()] = true
	}

	// Add any instances we've been told about but haven't yet shown
	// up in the instance list.
	for _, inst := range ensureInsts {
		id := inst.Id()
		if !found[id] {
			insts = append(insts, inst)
			found[id] = true
		}
	}
	return env.StopInstances(insts)
}

// OpenPorts is specified in the Environ interface.
func (env *azureEnviron) OpenPorts(ports []instance.Port) error {
	panic("unimplemented")
}

// ClosePorts is specified in the Environ interface.
func (env *azureEnviron) ClosePorts(ports []instance.Port) error {
	panic("unimplemented")
}

// Ports is specified in the Environ interface.
func (env *azureEnviron) Ports() ([]instance.Port, error) {
	panic("unimplemented")
}

// Provider is specified in the Environ interface.
func (env *azureEnviron) Provider() environs.EnvironProvider {
	panic("unimplemented")
}

// azureManagementContext wraps two things: a gwacl.ManagementAPI (effectively
// a session on the Azure management API) and a tempCertFile, which keeps track
// of the temporary certificate file that needs to be deleted once we're done
// with this particular session.
// Since it embeds *gwacl.ManagementAPI, you can use it much as if it were a
// pointer to a ManagementAPI object.  Just don't forget to release it after
// use.
type azureManagementContext struct {
	*gwacl.ManagementAPI
	certFile *tempCertFile
}

// getManagementAPI obtains a context object for interfacing with Azure's
// management API.
// For now, each invocation just returns a separate object.  This is probably
// wasteful (each context gets its own SSL connection) and may need optimizing
// later.
func (env *azureEnviron) getManagementAPI() (*azureManagementContext, error) {
	snap := env.getSnapshot()
	subscription := snap.ecfg.ManagementSubscriptionId()
	certData := snap.ecfg.ManagementCertificate()
	certFile, err := newTempCertFile([]byte(certData))
	if err != nil {
		return nil, err
	}
	// After this point, if we need to leave prematurely, we should clean
	// up that certificate file.
	mgtAPI, err := gwacl.NewManagementAPI(subscription, certFile.Path())
	if err != nil {
		certFile.Delete()
		return nil, err
	}
	context := azureManagementContext{
		ManagementAPI: mgtAPI,
		certFile:      certFile,
	}
	return &context, nil
}

// releaseManagementAPI frees up a context object obtained through
// getManagementAPI.
func (env *azureEnviron) releaseManagementAPI(context *azureManagementContext) {
	// Be tolerant to incomplete context objects, in case we ever get
	// called during cleanup of a failed attempt to create one.
	if context == nil || context.certFile == nil {
		return
	}
	// For now, all that needs doing is to delete the temporary certificate
	// file.  We may do cleverer things later, such as connection pooling
	// where this method returns a context to the pool.
	context.certFile.Delete()
}

// getStorageContext obtains a context object for interfacing with Azure's
// storage API.
// For now, each invocation just returns a separate object.  This is probably
// wasteful (each context gets its own SSL connection) and may need optimizing
// later.
func (env *azureEnviron) getStorageContext() (*gwacl.StorageContext, error) {
	ecfg := env.getSnapshot().ecfg
	context := gwacl.StorageContext{
		Account: ecfg.StorageAccountName(),
		Key:     ecfg.StorageAccountKey(),
	}
	// There is currently no way for this to fail.
	return &context, nil
}

// getPublicStorageContext obtains a context object for interfacing with
// Azure's storage API (public storage).
func (env *azureEnviron) getPublicStorageContext() (*gwacl.StorageContext, error) {
	ecfg := env.getSnapshot().ecfg
	context := gwacl.StorageContext{
		Account: ecfg.PublicStorageAccountName(),
		Key:     "", // Empty string means anonymous access.
	}
	// There is currently no way for this to fail.
	return &context, nil
}
