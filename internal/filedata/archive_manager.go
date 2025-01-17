package filedata

import (
	"io"
	"os"
	"sync"
	"time"

	"github.com/launchdarkly/go-sdk-common/v3/ldlog"
	"github.com/launchdarkly/ld-relay/v8/config"
)

const (
	// This value was chosen as a default after switching from file-watcher event-based monitoring to simple polling.
	// This idea is that polling should react fairly quickly to changes, just as the event-based system did to preserve
	// any use-cases that relied on it. In practice, much longer intervals could likely be chosen.
	defaultMonitoringInterval = 1 * time.Second
)

// ArchiveManager manages the file data source.
//
// That includes reading and unarchiving the data file, watching for changes to the file, and maintaining the
// last known state of the data so that it can determine what environmennts if any are affected by a change.
//
// Relay provides an implementation of the UpdateHandler interface which will be called for all changes that
// it needs to know about.
type ArchiveManager struct {
	filePath           string
	monitoringInterval time.Duration
	handler            UpdateHandler
	lastKnownEnvs      map[config.EnvironmentID]environmentMetadata
	loggers            ldlog.Loggers
	closeCh            chan struct{}
	closeOnce          sync.Once
}

// ArchiveManagerInterface is an interface containing the public methods of ArchiveManager. This is used
// for test stubbing.
type ArchiveManagerInterface interface {
	io.Closer
}

// NewArchiveManager creates the ArchiveManager instance and attempts to read the initial file data.
//
// If successful, it calls handler.AddEnvironment() for each environment configured in the file, and also
// starts a file watcher to detect updates to the file.
func NewArchiveManager(
	filePath string,
	handler UpdateHandler,
	monitoringInterval time.Duration, // zero = use the default; we set a nonzero brief interval in unit tests
	loggers ldlog.Loggers,
) (*ArchiveManager, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return nil, errCannotOpenArchiveFile(filePath, err)
	}

	am := &ArchiveManager{
		filePath:           filePath,
		handler:            handler,
		monitoringInterval: monitoringInterval,
		lastKnownEnvs:      make(map[config.EnvironmentID]environmentMetadata),
		loggers:            loggers,
		closeCh:            make(chan struct{}),
	}
	if am.monitoringInterval == 0 {
		am.monitoringInterval = defaultMonitoringInterval
	}
	am.loggers.SetPrefix("[FileDataSource]")

	ar, err := newArchiveReader(filePath)
	if err != nil {
		return nil, err
	}
	defer ar.Close()

	am.updatedArchive(ar)
	go am.monitorForChanges(fileInfo)

	return am, nil
}

// Close shuts down the ArchiveManager.
func (am *ArchiveManager) Close() error {
	am.closeOnce.Do(func() {
		close(am.closeCh)
	})
	return nil
}

func (am *ArchiveManager) monitorForChanges(original os.FileInfo) {
	ticker := time.NewTicker(am.monitoringInterval)
	defer ticker.Stop()

	prevInfo := original

	am.loggers.Infof(logMsgMonitoringStarted, am.filePath, am.monitoringInterval, original.Size(), original.ModTime())

	for {
		select {
		case <-am.closeCh:
			return
		case <-ticker.C:
			nextInfo, err := os.Stat(am.filePath)
			if err != nil {
				if os.IsNotExist(err) {
					am.loggers.Errorf(logMsgReloadFileStatNotFound, am.filePath)
				} else {
					am.loggers.Errorf(logMsgReloadFileStatUnknownError, err)
				}
				continue
			}
			if fileMayHaveChanged(prevInfo, nextInfo) {
				am.loggers.Infof(logMsgFileChanged, am.filePath, nextInfo.Size(), nextInfo.ModTime())
				reader, err := newArchiveReader(am.filePath)
				if err != nil {
					// A failure here might be a real failure, or it might be that the file is being copied
					// over non-atomically so that we're seeing an invalid partial state.
					am.loggers.Warnf(logMsgReloadError, err.Error())
					continue
				}
				am.loggers.Warnf(logMsgReloadedData, am.filePath)
				am.updatedArchive(reader)
				reader.Close()
			} else {
				am.loggers.Debugf(logMsgFileNotChanged, am.filePath, nextInfo.Size(), nextInfo.ModTime())
			}

			prevInfo = nextInfo
		}
	}
}

func (am *ArchiveManager) updatedArchive(ar *archiveReader) {
	unusedEnvs := make(map[config.EnvironmentID]environmentMetadata)
	for envID, envData := range am.lastKnownEnvs {
		unusedEnvs[envID] = envData
	}
	envIDs := ar.GetEnvironmentIDs()
	if len(envIDs) == 0 {
		am.loggers.Warn(logMsgNoEnvs)
	}
	for _, envID := range envIDs {
		envMetadata, err := ar.GetEnvironmentMetadata(envID)
		if err != nil {
			am.loggers.Errorf(logMsgBadEnvData, envID)
			continue
		}
		envName := envMetadata.params.Identifiers.GetDisplayName()
		delete(unusedEnvs, envID)
		if old, found := am.lastKnownEnvs[envID]; found {
			// Updating an existing environment
			if old.dataID == envMetadata.dataID && old.version == envMetadata.version {
				// Neither the metadata nor the SDK data has changed
				continue
			}
			ae := ArchiveEnvironment{Params: envMetadata.params}
			if old.dataID != envMetadata.dataID {
				// Reload the SDK data only if it has changed
				ae.SDKData, err = ar.GetEnvironmentSDKData(envID)
				if err != nil {
					am.loggers.Errorf(logMsgBadEnvData, envID)
					continue
				}
			}
			am.loggers.Infof(logMsgUpdateEnv, envID, envName)
			am.handler.UpdateEnvironment(ae)
		} else {
			// Adding a new environment
			ae := ArchiveEnvironment{Params: envMetadata.params}
			ae.SDKData, err = ar.GetEnvironmentSDKData(envID)
			if err != nil {
				am.loggers.Errorf(logMsgBadEnvData, envID)
				continue
			}
			am.loggers.Infof(logMsgAddEnv, envID, envName)
			am.handler.AddEnvironment(ae)
		}
		am.lastKnownEnvs[envID] = envMetadata
	}
	for envID, envData := range unusedEnvs {
		// Delete any environments that are no longer in the file
		am.loggers.Infof(logMsgDeleteEnv, envID, envData.params.Identifiers.GetDisplayName())
		delete(am.lastKnownEnvs, envID)
		am.handler.DeleteEnvironment(envID, envData.params.Identifiers.FilterKey)
	}
}

func fileMayHaveChanged(oldInfo, newInfo os.FileInfo) bool {
	return oldInfo.ModTime() != newInfo.ModTime() || oldInfo.Size() != newInfo.Size()
}
