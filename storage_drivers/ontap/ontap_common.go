// Copyright 2018 NetApp, Inc. All Rights Reserved.

package ontap

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	trident "github.com/netapp/trident/config"
	"github.com/netapp/trident/storage"
	sa "github.com/netapp/trident/storage_attribute"
	drivers "github.com/netapp/trident/storage_drivers"
	"github.com/netapp/trident/storage_drivers/ontap/api"
	"github.com/netapp/trident/storage_drivers/ontap/api/azgo"
	"github.com/netapp/trident/utils"
)

const (
	LSMirrorIdleTimeoutSecs      = 30
	MinimumVolumeSizeBytes       = 20971520 // 20 MiB
	HousekeepingStartupDelaySecs = 10
)

type Telemetry struct {
	trident.Telemetry
	Plugin        string        `json:"plugin"`
	SVM           string        `json:"svm"`
	StoragePrefix string        `json:"storagePrefix"`
	Driver        StorageDriver `json:"-"`
	done          chan struct{} `json:"-"`
	ticker        *time.Ticker  `json:"-"`
}

type StorageDriver interface {
	GetConfig() *drivers.OntapStorageDriverConfig
	GetAPI() *api.Client
	GetTelemetry() *Telemetry
	Name() string
}

// InitializeOntapConfig parses the ONTAP config, mixing in the specified common config.
func InitializeOntapConfig(
	context trident.DriverContext, configJSON string, commonConfig *drivers.CommonStorageDriverConfig,
) (*drivers.OntapStorageDriverConfig, error) {

	if commonConfig.DebugTraceFlags["method"] {
		fields := log.Fields{"Method": "InitializeOntapConfig", "Type": "ontap_common"}
		log.WithFields(fields).Debug(">>>> InitializeOntapConfig")
		defer log.WithFields(fields).Debug("<<<< InitializeOntapConfig")
	}

	commonConfig.DriverContext = context

	config := &drivers.OntapStorageDriverConfig{}
	config.CommonStorageDriverConfig = commonConfig

	// decode configJSON into OntapStorageDriverConfig object
	err := json.Unmarshal([]byte(configJSON), &config)
	if err != nil {
		return nil, fmt.Errorf("could not decode JSON configuration: %v", err)
	}

	return config, nil
}

func NewOntapTelemetry(d StorageDriver) *Telemetry {
	t := &Telemetry{
		Telemetry:     trident.OrchestratorTelemetry,
		Plugin:        d.Name(),
		SVM:           d.GetConfig().SVM,
		StoragePrefix: *d.GetConfig().StoragePrefix,
		Driver:        d,
		done:          make(chan struct{}),
	}

	usageHeartbeat := d.GetConfig().UsageHeartbeat
	heartbeatIntervalInHours := 24.0 // default to 24 hours
	if usageHeartbeat != "" {
		f, err := strconv.ParseFloat(usageHeartbeat, 64)
		if err != nil {
			log.WithField("interval", usageHeartbeat).Warnf("Invalid heartbeat interval. %v", err)
		} else {
			heartbeatIntervalInHours = f
		}
	}
	log.WithField("intervalHours", heartbeatIntervalInHours).Debug("Configured EMS heartbeat.")

	durationInHours := time.Millisecond * time.Duration(MSecPerHour*heartbeatIntervalInHours)
	if durationInHours > 0 {
		t.ticker = time.NewTicker(durationInHours)
	}
	return t
}

// Start starts the flow of ASUP messages for the driver
// These messages can be viewed via filer::> event log show -severity NOTICE.
func (t *Telemetry) Start() {
	go func() {
		time.Sleep(HousekeepingStartupDelaySecs * time.Second)
		EMSHeartbeat(t.Driver)
		for {
			select {
			case tick := <-t.ticker.C:
				log.WithFields(log.Fields{
					"tick":   tick,
					"driver": t.Driver.Name(),
				}).Debug("Sending EMS heartbeat.")
				EMSHeartbeat(t.Driver)
			case <-t.done:
				log.WithFields(log.Fields{
					"driver": t.Driver.Name(),
				}).Debugf("Shut down EMS logs for the driver.")
				return
			}
		}
	}()
}

func (t *Telemetry) Stop() {
	if t.ticker != nil {
		t.ticker.Stop()
	}
	close(t.done)
}

// InitializeOntapDriver sets up the API client and performs all other initialization tasks
// that are common to all the ONTAP drivers.
func InitializeOntapDriver(config *drivers.OntapStorageDriverConfig) (*api.Client, error) {

	if config.DebugTraceFlags["method"] {
		fields := log.Fields{"Method": "InitializeOntapDriver", "Type": "ontap_common"}
		log.WithFields(fields).Debug(">>>> InitializeOntapDriver")
		defer log.WithFields(fields).Debug("<<<< InitializeOntapDriver")
	}

	addressesFromHostname, err := net.LookupHost(config.ManagementLIF)
	if err != nil {
		log.WithField("ManagementLIF", config.ManagementLIF).Error("Host lookup failed for ManagementLIF. ", err)
		return nil, err
	}

	log.WithFields(log.Fields{
		"hostname":  config.ManagementLIF,
		"addresses": addressesFromHostname,
	}).Debug("Addresses found from ManagementLIF lookup.")

	// Get the API client
	client, err := InitializeOntapAPI(config)
	if err != nil {
		return nil, fmt.Errorf("could not create Data ONTAP API client: %v", err)
	}

	// Make sure we're using a valid ONTAP version
	ontapi, err := client.SystemGetOntapiVersion()
	if err != nil {
		return nil, fmt.Errorf("could not determine Data ONTAP API version: %v", err)
	}
	if !client.SupportsFeature(api.MinimumONTAPIVersion) {
		return nil, errors.New("ONTAP 8.3 or later is required")
	}
	log.WithField("Ontapi", ontapi).Debug("ONTAP API version.")

	// Log cluster node serial numbers if we can get them
	config.SerialNumbers, err = client.ListNodeSerialNumbers()
	if err != nil {
		log.Warnf("Could not determine controller serial numbers. %v", err)
	} else {
		log.WithFields(log.Fields{
			"serialNumbers": strings.Join(config.SerialNumbers, ","),
		}).Info("Controller serial numbers.")
	}

	// Load default config parameters
	err = PopulateConfigurationDefaults(config)
	if err != nil {
		return nil, fmt.Errorf("could not populate configuration defaults: %v", err)
	}

	return client, nil
}

// InitializeOntapAPI returns an ontap.Client ZAPI client.  If the SVM isn't specified in the config
// file, this method attempts to derive the one to use.
func InitializeOntapAPI(config *drivers.OntapStorageDriverConfig) (*api.Client, error) {

	if config.DebugTraceFlags["method"] {
		fields := log.Fields{"Method": "InitializeOntapAPI", "Type": "ontap_common"}
		log.WithFields(fields).Debug(">>>> InitializeOntapAPI")
		defer log.WithFields(fields).Debug("<<<< InitializeOntapAPI")
	}

	client := api.NewClient(api.ClientConfig{
		ManagementLIF:   config.ManagementLIF,
		SVM:             config.SVM,
		Username:        config.Username,
		Password:        config.Password,
		DebugTraceFlags: config.DebugTraceFlags,
	})

	if config.SVM != "" {
		log.WithField("SVM", config.SVM).Debug("Using specified SVM.")
		return client, nil
	}

	// Use VserverGetIterRequest to populate config.SVM if it wasn't specified and we can derive it
	vserverResponse, err := client.VserverGetIterRequest()
	if err = api.GetError(vserverResponse, err); err != nil {
		return nil, fmt.Errorf("error enumerating SVMs: %v", err)
	}

	if vserverResponse.Result.NumRecords() != 1 {
		return nil, errors.New("cannot derive SVM to use; please specify SVM in config file")
	}

	// Update everything to use our derived SVM
	config.SVM = vserverResponse.Result.AttributesList()[0].VserverName()
	client = api.NewClient(api.ClientConfig{
		ManagementLIF:   config.ManagementLIF,
		SVM:             config.SVM,
		Username:        config.Username,
		Password:        config.Password,
		DebugTraceFlags: config.DebugTraceFlags,
	})
	log.WithField("SVM", config.SVM).Debug("Using derived SVM.")

	return client, nil
}

// ValidateAggregate returns an error if the configured aggregate is not available to the Vserver.
func ValidateAggregate(api *api.Client, config *drivers.OntapStorageDriverConfig) error {

	if config.DebugTraceFlags["method"] {
		fields := log.Fields{"Method": "ValidateAggregate", "Type": "ontap_common"}
		log.WithFields(fields).Debug(">>>> ValidateAggregate")
		defer log.WithFields(fields).Debug("<<<< ValidateAggregate")
	}

	if config.Aggregate == "" {
		return errors.New("no aggregate was specified in the config file")
	}

	// Get the aggregates assigned to the SVM.  There must be at least one!
	vserverAggrs, err := api.GetVserverAggregateNames()
	if err != nil {
		return err
	}
	if len(vserverAggrs) == 0 {
		return fmt.Errorf("SVM %s has no assigned aggregates", config.SVM)
	}

	for _, aggrName := range vserverAggrs {
		if aggrName == config.Aggregate {
			log.WithFields(log.Fields{
				"SVM":       config.SVM,
				"Aggregate": config.Aggregate,
			}).Debug("Found aggregate for SVM.")
			return nil
		}
	}

	return fmt.Errorf("aggregate %s does not exist or is not assigned to SVM %s", config.Aggregate, config.SVM)
}

// ValidateNASDriver contains the validation logic shared between ontap-nas and ontap-nas-economy.
func ValidateNASDriver(api *api.Client, config *drivers.OntapStorageDriverConfig) error {

	if config.DebugTraceFlags["method"] {
		fields := log.Fields{"Method": "ValidateNASDriver", "Type": "ontap_common"}
		log.WithFields(fields).Debug(">>>> ValidateNASDriver")
		defer log.WithFields(fields).Debug("<<<< ValidateNASDriver")
	}

	dataLIFs, err := api.NetInterfaceGetDataLIFs("nfs")
	if err != nil {
		return err
	}

	if len(dataLIFs) == 0 {
		return fmt.Errorf("no NAS data LIFs found on SVM %s", config.SVM)
	} else {
		log.WithField("dataLIFs", dataLIFs).Debug("Found NAS LIFs.")
	}

	// If they didn't set a LIF to use in the config, we'll set it to the first nfs LIF we happen to find
	if config.DataLIF == "" {
		config.DataLIF = dataLIFs[0]
	} else {
		err := ValidateDataLIFs(config, dataLIFs)
		if err != nil {
			return fmt.Errorf("data LIF validation failed: %v", err)
		}

	}

	if config.DriverContext == trident.ContextDocker {
		// Make sure the configured aggregate is available
		err = ValidateAggregate(api, config)
		if err != nil {
			return err
		}
	}

	return nil
}

func ValidateDataLIFs(config *drivers.OntapStorageDriverConfig, dataLIFs []string) error {

	addressesFromHostname, err := net.LookupHost(config.DataLIF)
	if err != nil {
		log.Error("Host lookup failed. ", err)
		return err
	}

	log.WithFields(log.Fields{
		"hostname":  config.DataLIF,
		"addresses": addressesFromHostname,
	}).Debug("Addresses found from hostname lookup.")

	for _, hostNameAddress := range addressesFromHostname {
		foundValidLIFAddress := false

	loop:
		for _, lifAddress := range dataLIFs {
			if lifAddress == hostNameAddress {
				foundValidLIFAddress = true
				break loop
			}
		}
		if foundValidLIFAddress {
			log.WithField("hostNameAddress", hostNameAddress).Debug("Found matching Data LIF.")
		} else {
			log.WithField("hostNameAddress", hostNameAddress).Debug("Could not find matching Data LIF.")
			return fmt.Errorf("could not find Data LIF for %s", hostNameAddress)
		}

	}

	return nil
}

const DefaultSpaceReserve = "none"
const DefaultSnapshotPolicy = "none"
const DefaultUnixPermissions = "---rwxrwxrwx"
const DefaultSnapshotDir = "false"
const DefaultExportPolicy = "default"
const DefaultSecurityStyle = "unix"
const DefaultNfsMountOptions = "-o nfsvers=3"
const DefaultSplitOnClone = "false"
const DefaultFileSystemType = "ext4"
const DefaultEncryption = "false"

// PopulateConfigurationDefaults fills in default values for configuration settings if not supplied in the config file
func PopulateConfigurationDefaults(config *drivers.OntapStorageDriverConfig) error {

	if config.DebugTraceFlags["method"] {
		fields := log.Fields{"Method": "PopulateConfigurationDefaults", "Type": "ontap_common"}
		log.WithFields(fields).Debug(">>>> PopulateConfigurationDefaults")
		defer log.WithFields(fields).Debug("<<<< PopulateConfigurationDefaults")
	}

	// Ensure the default volume size is valid, using a "default default" of 1G if not set
	if config.Size == "" {
		config.Size = drivers.DefaultVolumeSize
	} else {
		_, err := utils.ConvertSizeToBytes(config.Size)
		if err != nil {
			return fmt.Errorf("invalid config value for default volume size: %v", err)
		}
	}

	if config.StoragePrefix == nil {
		prefix := drivers.GetDefaultStoragePrefix(config.DriverContext)
		config.StoragePrefix = &prefix
	}

	if config.SpaceReserve == "" {
		config.SpaceReserve = DefaultSpaceReserve
	}

	if config.SnapshotPolicy == "" {
		config.SnapshotPolicy = DefaultSnapshotPolicy
	}

	if config.UnixPermissions == "" {
		config.UnixPermissions = DefaultUnixPermissions
	}

	if config.SnapshotDir == "" {
		config.SnapshotDir = DefaultSnapshotDir
	}

	if config.ExportPolicy == "" {
		config.ExportPolicy = DefaultExportPolicy
	}

	if config.SecurityStyle == "" {
		config.SecurityStyle = DefaultSecurityStyle
	}

	if config.NfsMountOptions == "" {
		config.NfsMountOptions = DefaultNfsMountOptions
	}

	if config.SplitOnClone == "" {
		config.SplitOnClone = DefaultSplitOnClone
	} else {
		_, err := strconv.ParseBool(config.SplitOnClone)
		if err != nil {
			return fmt.Errorf("invalid boolean value for splitOnClone: %v", err)
		}
	}

	if config.FileSystemType == "" {
		config.FileSystemType = DefaultFileSystemType
	}

	if config.Encryption == "" {
		config.Encryption = DefaultEncryption
	}

	log.WithFields(log.Fields{
		"StoragePrefix":   *config.StoragePrefix,
		"SpaceReserve":    config.SpaceReserve,
		"SnapshotPolicy":  config.SnapshotPolicy,
		"UnixPermissions": config.UnixPermissions,
		"SnapshotDir":     config.SnapshotDir,
		"ExportPolicy":    config.ExportPolicy,
		"SecurityStyle":   config.SecurityStyle,
		"NfsMountOptions": config.NfsMountOptions,
		"SplitOnClone":    config.SplitOnClone,
		"FileSystemType":  config.FileSystemType,
		"Encryption":      config.Encryption,
		"Size":            config.Size,
	}).Debugf("Configuration defaults")

	return nil
}

// ValidateEncryptionAttribute returns true/false if encryption is being requested of a backend that
// supports NetApp Volume Encryption, and nil otherwise so that the ZAPIs may be sent without
// any reference to encryption.
func ValidateEncryptionAttribute(encryption string, client *api.Client) (*bool, error) {

	enableEncryption, err := strconv.ParseBool(encryption)
	if err != nil {
		return nil, fmt.Errorf("invalid boolean value for encryption: %v", err)
	}

	if client.SupportsFeature(api.NetAppVolumeEncryption) {
		return &enableEncryption, nil
	} else {
		if enableEncryption {
			return nil, errors.New("encrypted volumes are not supported on this storage backend")
		} else {
			return nil, nil
		}
	}
}

func GetVolumeSize(sizeBytes uint64, config drivers.OntapStorageDriverConfig) (uint64, error) {

	if sizeBytes == 0 {
		defaultSize, _ := utils.ConvertSizeToBytes(config.Size)
		sizeBytes, _ = strconv.ParseUint(defaultSize, 10, 64)
	}
	if sizeBytes < MinimumVolumeSizeBytes {
		return 0, fmt.Errorf("requested volume size (%d bytes) is too small; "+
			"the minimum volume size is %d bytes", sizeBytes, MinimumVolumeSizeBytes)
	}
	return sizeBytes, nil
}

// EMSHeartbeat logs an ASUP message on a timer
// view them via filer::> event log show -severity NOTICE
func EMSHeartbeat(driver StorageDriver) {

	// log an informational message on a timer
	hostname, err := os.Hostname()
	if err != nil {
		log.Warnf("Could not determine hostname. %v", err)
		hostname = "unknown"
	}

	message, _ := json.Marshal(driver.GetTelemetry())

	emsResponse, err := driver.GetAPI().EmsAutosupportLog(
		strconv.Itoa(drivers.ConfigVersion), false, "heartbeat", hostname,
		string(message), 1, trident.OrchestratorName, 5)

	if err = api.GetError(emsResponse, err); err != nil {
		log.WithFields(log.Fields{
			"driver": driver.Name(),
			"error":  err,
		}).Error("Error logging EMS message.")
	} else {
		log.WithField("driver", driver.Name()).Info("Logged EMS message.")
	}
}

const MSecPerHour = 1000 * 60 * 60 // millis * seconds * minutes

// Create a volume clone
func CreateOntapClone(
	name, source, snapshot string, split bool, config *drivers.OntapStorageDriverConfig, client *api.Client,
) error {

	if config.DebugTraceFlags["method"] {
		fields := log.Fields{
			"Method":   "CreateOntapClone",
			"Type":     "ontap_common",
			"name":     name,
			"source":   source,
			"snapshot": snapshot,
			"split":    split,
		}
		log.WithFields(fields).Debug(">>>> CreateOntapClone")
		defer log.WithFields(fields).Debug("<<<< CreateOntapClone")
	}

	// If the specified volume already exists, return an error
	volExists, err := client.VolumeExists(name)
	if err != nil {
		return fmt.Errorf("error checking for existing volume: %v", err)
	}
	if volExists {
		return fmt.Errorf("volume %s already exists", name)
	}

	// If no specific snapshot was requested, create one
	if snapshot == "" {
		// This is golang being stupid: https://golang.org/pkg/time/#Time.Format
		snapshot = time.Now().UTC().Format("20060102T150405Z")
		snapResponse, err := client.SnapshotCreate(snapshot, source)
		if err = api.GetError(snapResponse, err); err != nil {
			return fmt.Errorf("error creating snapshot: %v", err)
		}
	}

	// Create the clone based on a snapshot
	cloneResponse, err := client.VolumeCloneCreate(name, source, snapshot)
	if err != nil {
		return fmt.Errorf("error creating clone: %v", err)
	}
	if zerr := api.NewZapiError(cloneResponse); !zerr.IsPassed() {
		if zerr.Code() == azgo.EOBJECTNOTFOUND {
			return fmt.Errorf("snapshot %s does not exist in volume %s", snapshot, source)
		} else {
			return fmt.Errorf("error creating clone: %v", zerr)
		}
	}

	if config.StorageDriverName == drivers.OntapNASStorageDriverName {
		// Mount the new volume
		mountResponse, err := client.VolumeMount(name, "/"+name)
		if err = api.GetError(mountResponse, err); err != nil {
			return fmt.Errorf("error mounting volume to junction: %v", err)
		}
	}

	// Split the clone if requested
	if split {
		splitResponse, err := client.VolumeCloneSplitStart(name)
		if err = api.GetError(splitResponse, err); err != nil {
			return fmt.Errorf("error splitting clone: %v", err)
		}
	}

	return nil
}

// Return the list of snapshots associated with the named volume
func GetSnapshotList(name string, config *drivers.OntapStorageDriverConfig, client *api.Client) ([]storage.Snapshot, error) {

	if config.DebugTraceFlags["method"] {
		fields := log.Fields{
			"Method": "GetSnapshotList",
			"Type":   "ontap_common",
			"name":   name,
		}
		log.WithFields(fields).Debug(">>>> GetSnapshotList")
		defer log.WithFields(fields).Debug("<<<< GetSnapshotList")
	}

	snapResponse, err := client.SnapshotGetByVolume(name)
	if err = api.GetError(snapResponse, err); err != nil {
		return nil, fmt.Errorf("error enumerating snapshots: %v", err)
	}

	log.Debugf("Returned %v snapshots.", snapResponse.Result.NumRecords())
	snapshots := []storage.Snapshot{}

	// AttributesList() returns []SnapshotInfoType
	for _, snap := range snapResponse.Result.AttributesList() {

		log.WithFields(log.Fields{
			"name":       snap.Name(),
			"accessTime": snap.AccessTime(),
		}).Debug("Snapshot")

		// Time format: yyyy-mm-ddThh:mm:ssZ
		snapTime := time.Unix(int64(snap.AccessTime()), 0).UTC().Format("2006-01-02T15:04:05Z")

		snapshots = append(snapshots, storage.Snapshot{snap.Name(), snapTime})
	}

	return snapshots, nil
}

// Return the list of volumes associated with the tenant
func GetVolumeList(client *api.Client, config *drivers.OntapStorageDriverConfig) ([]string, error) {

	if config.DebugTraceFlags["method"] {
		fields := log.Fields{"Method": "GetVolumeList", "Type": "ontap_common"}
		log.WithFields(fields).Debug(">>>> GetVolumeList")
		defer log.WithFields(fields).Debug("<<<< GetVolumeList")
	}

	prefix := *config.StoragePrefix

	volResponse, err := client.VolumeList(prefix)
	if err = api.GetError(volResponse, err); err != nil {
		return nil, fmt.Errorf("error enumerating volumes: %v", err)
	}

	var volumes []string

	// AttributesList() returns []VolumeAttributesType
	for _, volume := range volResponse.Result.AttributesList() {
		volIDAttrs := volume.VolumeIdAttributes()
		volName := string(volIDAttrs.Name())[len(prefix):]
		volumes = append(volumes, volName)
	}

	return volumes, nil
}

// GetVolume checks for the existence of a volume.  It returns nil if the volume
// exists and an error if it does not (or the API call fails).
func GetVolume(name string, client *api.Client, config *drivers.OntapStorageDriverConfig) error {

	if config.DebugTraceFlags["method"] {
		fields := log.Fields{"Method": "GetVolume", "Type": "ontap_common"}
		log.WithFields(fields).Debug(">>>> GetVolume")
		defer log.WithFields(fields).Debug("<<<< GetVolume")
	}

	volExists, err := client.VolumeExists(name)
	if err != nil {
		return fmt.Errorf("error checking for existing volume: %v", err)
	}
	if !volExists {
		log.WithField("flexvol", name).Debug("Flexvol not found.")
		return fmt.Errorf("volume %s does not exist", name)
	}

	return nil
}

// MountVolume accepts the mount info for an NFS share and mounts it on the local host.
func MountVolume(exportPath, mountpoint string, config *drivers.OntapStorageDriverConfig) error {

	if config.DebugTraceFlags["method"] {
		fields := log.Fields{
			"Method":     "MountVolume",
			"Type":       "ontap_common",
			"exportPath": exportPath,
			"mountpoint": mountpoint,
		}
		log.WithFields(fields).Debug(">>>> MountVolume")
		defer log.WithFields(fields).Debug("<<<< MountVolume")
	}

	nfsMountOptions := config.NfsMountOptions

	// Do the mount
	var cmd string
	switch runtime.GOOS {
	case utils.Linux:
		cmd = fmt.Sprintf("mount -v %s %s %s", nfsMountOptions, exportPath, mountpoint)
	case utils.Darwin:
		cmd = fmt.Sprintf("mount -v -o rw %s -t nfs %s %s", nfsMountOptions, exportPath, mountpoint)
	default:
		return fmt.Errorf("unsupported operating system: %v", runtime.GOOS)
	}

	log.WithField("command", cmd).Debug("Mounting volume.")

	if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
		log.WithField("output", string(out)).Debug("Mount failed.")
		return fmt.Errorf("error mounting NFS volume %v on mountpoint %v: %v", exportPath, mountpoint, err)
	}

	return nil
}

// UnmountVolume unmounts the volume mounted on the specified mountpoint.
func UnmountVolume(mountpoint string, config *drivers.OntapStorageDriverConfig) error {

	if config.DebugTraceFlags["method"] {
		fields := log.Fields{
			"Method":     "UnmountVolume",
			"Type":       "ontap_common",
			"mountpoint": mountpoint,
		}
		log.WithFields(fields).Debug(">>>> UnmountVolume")
		defer log.WithFields(fields).Debug("<<<< UnmountVolume")
	}

	cmd := fmt.Sprintf("umount %s", mountpoint)
	log.WithField("command", cmd).Debug("Unmounting volume.")

	if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
		log.WithField("output", string(out)).Debug("Unmount failed.")
		return fmt.Errorf("error unmounting NFS volume from mountpoint %v: %v", mountpoint, err)
	}

	return nil
}

// UpdateLoadSharingMirrors checks for the present of LS mirrors on the SVM root volume, and if
// present, starts an update and waits for them to become idle.
func UpdateLoadSharingMirrors(client *api.Client) {

	// We care about LS mirrors on the SVM root volume, so get the root volume name
	rootVolumeResponse, err := client.VolumeGetRootName()
	if err = api.GetError(rootVolumeResponse, err); err != nil {
		log.Warnf("Error getting SVM root volume. %v", err)
		return
	}
	rootVolume := rootVolumeResponse.Result.Volume()

	// Check for LS mirrors on the SVM root volume
	mirrorGetResponse, err := client.SnapmirrorGetLoadSharingMirrors(rootVolume)
	if err = api.GetError(rootVolumeResponse, err); err != nil {
		log.Warnf("Error getting load-sharing mirrors for SVM root volume. %v", err)
		return
	}
	if mirrorGetResponse.Result.NumRecords() == 0 {
		// None found, so nothing more to do
		log.WithField("rootVolume", rootVolume).Debug("SVM root volume has no load-sharing mirrors.")
		return
	}

	// One or more LS mirrors found, so issue an update
	mirrorSourceLocation := mirrorGetResponse.Result.AttributesList()[0].SourceLocation()
	_, err = client.SnapmirrorUpdateLoadSharingMirrors(mirrorSourceLocation)
	if err = api.GetError(rootVolumeResponse, err); err != nil {
		log.Warnf("Error updating load-sharing mirrors for SVM root volume. %v", err)
		return
	}

	// Wait for LS mirrors to become idle
	timeout := time.Now().Add(LSMirrorIdleTimeoutSecs * time.Second)
	for {
		time.Sleep(1 * time.Second)
		log.Debug("Load-sharing mirrors not yet idle, polling...")

		mirrorGetResponse, err = client.SnapmirrorGetLoadSharingMirrors(rootVolume)
		if err = api.GetError(rootVolumeResponse, err); err != nil {
			log.Warnf("Error getting load-sharing mirrors for SVM root volume. %v", err)
			break
		}
		if mirrorGetResponse.Result.NumRecords() == 0 {
			log.WithField("rootVolume", rootVolume).Debug("SVM root volume has no load-sharing mirrors.")
			break
		}

		// Ensure all mirrors are idle
		idle := true
		for _, mirror := range mirrorGetResponse.Result.AttributesList() {
			if mirror.RelationshipStatusPtr == nil || mirror.RelationshipStatus() != "idle" {
				idle = false
			}
		}
		if idle {
			log.Debug("Load-sharing mirrors idle.")
			break
		}

		// Don't wait forever
		if time.Now().After(timeout) {
			log.Warning("Load-sharing mirrors not yet idle, giving up.")
			break
		}
	}
}

type ontapPerformanceClass string

const (
	ontapHDD    ontapPerformanceClass = "hdd"
	ontapHybrid ontapPerformanceClass = "hybrid"
	ontapSSD    ontapPerformanceClass = "ssd"
)

var ontapPerformanceClasses = map[ontapPerformanceClass]map[string]sa.Offer{
	ontapHDD:    {sa.Media: sa.NewStringOffer(sa.HDD)},
	ontapHybrid: {sa.Media: sa.NewStringOffer(sa.Hybrid)},
	ontapSSD:    {sa.Media: sa.NewStringOffer(sa.SSD)},
}

// getStorageBackendSpecsCommon discovers the aggregates assigned to the configured SVM, and it updates the specified Backend
// object with StoragePools and their associated attributes.
func getStorageBackendSpecsCommon(
	d StorageDriver, backend *storage.Backend, poolAttributes map[string]sa.Offer,
) (err error) {

	client := d.GetAPI()
	config := d.GetConfig()
	driverName := d.Name()

	// Handle panics from the API layer
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("unable to inspect ONTAP backend: %v\nStack trace:\n%s", r, debug.Stack())
		}
	}()

	// Get the aggregates assigned to the SVM.  There must be at least one!
	vserverAggrs, err := client.GetVserverAggregateNames()
	if err != nil {
		return
	}
	if len(vserverAggrs) == 0 {
		err = fmt.Errorf("SVM %s has no assigned aggregates", config.SVM)
		return
	}

	log.WithFields(log.Fields{
		"svm":   config.SVM,
		"pools": vserverAggrs,
	}).Debug("Read storage pools assigned to SVM.")

	// Define a storage pool for each of the SVM's aggregates
	storagePools := make(map[string]*storage.Pool)
	for _, aggrName := range vserverAggrs {
		storagePools[aggrName] = storage.NewStoragePool(backend, aggrName)
	}

	// Use all assigned aggregates unless 'aggregate' is set in the config
	if config.Aggregate != "" {

		// Make sure the configured aggregate is available to the SVM
		if _, ok := storagePools[config.Aggregate]; !ok {
			err = fmt.Errorf("the assigned aggregates for SVM %s do not include the configured aggregate %s",
				config.SVM, config.Aggregate)
			return
		}

		log.WithFields(log.Fields{
			"driverName": driverName,
			"aggregate":  config.Aggregate,
		}).Debug("Provisioning will be restricted to the aggregate set in the backend config.")

		storagePools = make(map[string]*storage.Pool)
		storagePools[config.Aggregate] = storage.NewStoragePool(backend, config.Aggregate)
	}

	// Update pools with aggregate info (i.e. MediaType) using the best means possible
	var aggrErr error
	if client.SupportsFeature(api.VServerShowAggr) {
		aggrErr = getVserverAggregateAttributes(d, &storagePools)
	} else {
		aggrErr = getClusterAggregateAttributes(d, &storagePools)
	}

	if zerr, ok := aggrErr.(api.ZapiError); ok && zerr.IsScopeError() {
		log.WithFields(log.Fields{
			"username": config.Username,
		}).Warn("User has insufficient privileges to obtain aggregate info. " +
			"Storage classes with physical attributes such as 'media' will not match pools on this backend.")
	} else if aggrErr != nil {
		log.Errorf("Could not obtain aggregate info; storage classes with physical attributes such as 'media' will"+
			" not match pools on this backend: %v.", aggrErr)
	}

	// Add attributes common to each pool and register pools with backend
	for _, pool := range storagePools {

		for attrName, offer := range poolAttributes {
			pool.Attributes[attrName] = offer
		}

		backend.AddStoragePool(pool)
	}

	return
}

// getVserverAggregateAttributes gets pool attributes using vserver-show-aggr-get-iter, which will only succeed on Data ONTAP 9 and later.
// If the aggregate attributes are read successfully, the pools passed to this function are updated accordingly.
func getVserverAggregateAttributes(d StorageDriver, storagePools *map[string]*storage.Pool) error {

	result, err := d.GetAPI().VserverShowAggrGetIterRequest()
	if err != nil {
		return err
	}
	if zerr := api.NewZapiError(result.Result); !zerr.IsPassed() {
		return zerr
	}

	for _, aggr := range result.Result.AttributesList() {
		aggrName := string(aggr.AggregateName())
		aggrType := aggr.AggregateType()

		// Find matching pool.  There are likely more aggregates in the cluster than those assigned to this backend's SVM.
		pool, ok := (*storagePools)[aggrName]
		if !ok {
			continue
		}

		// Get the storage attributes (i.e. MediaType) corresponding to the aggregate type
		storageAttrs, ok := ontapPerformanceClasses[ontapPerformanceClass(aggrType)]
		if !ok {
			log.WithFields(log.Fields{
				"aggregate": aggrName,
				"mediaType": aggrType,
			}).Warn("Aggregate has unknown media type.")

			continue
		}

		log.WithFields(log.Fields{
			"aggregate": aggrName,
			"mediaType": aggrType,
		}).Debug("Read aggregate attributes.")

		// Update the pool with the aggregate storage attributes
		for attrName, attr := range storageAttrs {
			pool.Attributes[attrName] = attr
		}
	}

	return nil
}

// getClusterAggregateAttributes gets pool attributes using aggr-get-iter, which will only succeed for cluster-scoped users
// with adequate permissions.  If the aggregate attributes are read successfully, the pools passed to this function are updated
// accordingly.
func getClusterAggregateAttributes(d StorageDriver, storagePools *map[string]*storage.Pool) error {

	result, err := d.GetAPI().AggrGetIterRequest()
	if err != nil {
		return err
	}
	if zerr := api.NewZapiError(result.Result); !zerr.IsPassed() {
		return zerr
	}

	for _, aggr := range result.Result.AttributesList() {
		aggrName := aggr.AggregateName()
		aggrRaidAttrs := aggr.AggrRaidAttributes()
		aggrType := aggrRaidAttrs.AggregateType()

		// Find matching pool.  There are likely more aggregates in the cluster than those assigned to this backend's SVM.
		pool, ok := (*storagePools)[aggrName]
		if !ok {
			continue
		}

		// Get the storage attributes (i.e. MediaType) corresponding to the aggregate type
		storageAttrs, ok := ontapPerformanceClasses[ontapPerformanceClass(aggrType)]
		if !ok {
			log.WithFields(log.Fields{
				"aggregate": aggrName,
				"mediaType": aggrType,
			}).Warn("Aggregate has unknown media type.")

			continue
		}

		log.WithFields(log.Fields{
			"aggregate": aggrName,
			"mediaType": aggrType,
		}).Debug("Read aggregate attributes.")

		// Update the pool with the aggregate storage attributes
		for attrName, attr := range storageAttrs {
			pool.Attributes[attrName] = attr
		}
	}

	return nil
}

func getVolumeOptsCommon(
	volConfig *storage.VolumeConfig,
	pool *storage.Pool,
	requests map[string]sa.Request,
) map[string]string {
	opts := make(map[string]string)
	if pool != nil {
		opts["aggregate"] = pool.Name
	}
	if provisioningTypeReq, ok := requests[sa.ProvisioningType]; ok {
		if p, ok := provisioningTypeReq.Value().(string); ok {
			if p == "thin" {
				opts["spaceReserve"] = "none"
			} else if p == "thick" {
				// p will equal "thick" here
				opts["spaceReserve"] = "volume"
			} else {
				log.WithFields(log.Fields{
					"provisioner":      "ONTAP",
					"method":           "getVolumeOptsCommon",
					"provisioningType": provisioningTypeReq.Value(),
				}).Warnf("Expected 'thick' or 'thin' for %s; ignoring.",
					sa.ProvisioningType)
			}
		} else {
			log.WithFields(log.Fields{
				"provisioner":      "ONTAP",
				"method":           "getVolumeOptsCommon",
				"provisioningType": provisioningTypeReq.Value(),
			}).Warnf("Expected string for %s; ignoring.", sa.ProvisioningType)
		}
	}
	if encryptionReq, ok := requests[sa.Encryption]; ok {
		if encryption, ok := encryptionReq.Value().(bool); ok {
			if encryption {
				opts["encryption"] = "true"
			}
		} else {
			log.WithFields(log.Fields{
				"provisioner": "ONTAP",
				"method":      "getVolumeOptsCommon",
				"encryption":  encryptionReq.Value(),
			}).Warnf("Expected bool for %s; ignoring.", sa.Encryption)
		}
	}
	if volConfig.SnapshotPolicy != "" {
		opts["snapshotPolicy"] = volConfig.SnapshotPolicy
	}
	if volConfig.UnixPermissions != "" {
		opts["unixPermissions"] = volConfig.UnixPermissions
	}
	if volConfig.SnapshotDir != "" {
		opts["snapshotDir"] = volConfig.SnapshotDir
	}
	if volConfig.ExportPolicy != "" {
		opts["exportPolicy"] = volConfig.ExportPolicy
	}
	if volConfig.SpaceReserve != "" {
		opts["spaceReserve"] = volConfig.SpaceReserve
	}
	if volConfig.SecurityStyle != "" {
		opts["securityStyle"] = volConfig.SecurityStyle
	}
	if volConfig.SplitOnClone != "" {
		opts["splitOnClone"] = volConfig.SplitOnClone
	}
	if volConfig.FileSystem != "" {
		opts["fileSystemType"] = volConfig.FileSystem
	}
	if volConfig.Encryption != "" {
		opts["encryption"] = volConfig.Encryption
	}

	return opts
}

func getInternalVolumeNameCommon(commonConfig *drivers.CommonStorageDriverConfig, name string) string {

	if trident.UsingPassthroughStore {
		// With a passthrough store, the name mapping must remain reversible
		return *commonConfig.StoragePrefix + name
	} else {
		// With an external store, any transformation of the name is fine
		internal := drivers.GetCommonInternalVolumeName(commonConfig, name)
		internal = strings.Replace(internal, "-", "_", -1)  // ONTAP disallows hyphens
		internal = strings.Replace(internal, ".", "_", -1)  // ONTAP disallows periods
		internal = strings.Replace(internal, "__", "_", -1) // Remove any double underscores
		return internal
	}
}

func createPrepareCommon(d storage.Driver, volConfig *storage.VolumeConfig) bool {

	volConfig.InternalName = d.GetInternalVolumeName(volConfig.Name)

	if volConfig.CloneSourceVolume != "" {
		volConfig.CloneSourceVolumeInternal =
			d.GetInternalVolumeName(volConfig.CloneSourceVolume)
	}

	return true
}

func getExternalConfig(config drivers.OntapStorageDriverConfig) interface{} {

	drivers.SanitizeCommonStorageDriverConfig(config.CommonStorageDriverConfig)

	return &struct {
		*drivers.CommonStorageDriverConfigExternal
		ManagementLIF string `json:"managementLIF"`
		DataLIF       string `json:"dataLIF"`
		IgroupName    string `json:"igroupName"`
		SVM           string `json:"svm"`
	}{
		CommonStorageDriverConfigExternal: drivers.GetCommonStorageDriverConfigExternal(
			config.CommonStorageDriverConfig,
		),
		ManagementLIF: config.ManagementLIF,
		DataLIF:       config.DataLIF,
		IgroupName:    config.IgroupName,
		SVM:           config.SVM,
	}
}
