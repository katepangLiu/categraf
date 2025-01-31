//go:build !no_logs

// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package auditor

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"flashcat.cloud/categraf/config"
	"flashcat.cloud/categraf/logs/message"
)

// DefaultRegistryFilename is the default registry filename
const DefaultRegistryFilename = "registry.json"

const defaultFlushPeriod = 1 * time.Second
const defaultCleanupPeriod = 300 * time.Second

// latest version of the API used by the auditor to retrieve the registry from disk.
const registryAPIVersion = 2

// Registry holds a list of offsets.
type Registry interface {
	GetOffset(identifier string) string
	GetTailingMode(identifier string) string
}

// A RegistryEntry represents an entry in the registry where we keep track
// of current offsets
type RegistryEntry struct {
	LastUpdated time.Time
	Offset      string
	TailingMode string
}

// JSONRegistry represents the registry that will be written on disk
type JSONRegistry struct {
	Version  int
	Registry map[string]RegistryEntry
}

// An Auditor handles messages successfully submitted to the intake
type Auditor interface {
	Registry
	Start()
	Stop()
	Channel() chan *message.Message
}

// A RegistryAuditor is storing the Auditor information using a registry.
type RegistryAuditor struct {
	chansMutex    sync.Mutex
	inputChan     chan *message.Message
	registry      map[string]*RegistryEntry
	registryPath  string
	registryMutex sync.Mutex
	entryTTL      time.Duration
	done          chan struct{}
}

// New returns an initialized Auditor
func New(runPath string, filename string, ttl time.Duration) *RegistryAuditor {
	return &RegistryAuditor{
		registryPath: filepath.Join(runPath, filename),
		entryTTL:     ttl,
	}
}

// Start starts the Auditor
func (a *RegistryAuditor) Start() {
	a.createChannels()
	a.registry = a.recoverRegistry()
	a.cleanupRegistry()
	go a.run()
}

// Stop stops the Auditor
func (a *RegistryAuditor) Stop() {
	a.closeChannels()
	a.cleanupRegistry()
	if err := a.flushRegistry(); err != nil {
		log.Println("W!", err)
	}
}

func (a *RegistryAuditor) createChannels() {
	a.chansMutex.Lock()
	defer a.chansMutex.Unlock()
	a.inputChan = make(chan *message.Message, config.ChanSize())
	a.done = make(chan struct{})
}

func (a *RegistryAuditor) closeChannels() {
	a.chansMutex.Lock()
	defer a.chansMutex.Unlock()
	if a.inputChan != nil {
		close(a.inputChan)
	}

	if a.done != nil {
		<-a.done
		a.done = nil
	}
	a.inputChan = nil
}

// Channel returns the channel to use to communicate with the auditor or nil
// if the auditor is currently stopped.
func (a *RegistryAuditor) Channel() chan *message.Message {
	return a.inputChan
}

// GetOffset returns the last committed offset for a given identifier,
// returns an empty string if it does not exist.
func (a *RegistryAuditor) GetOffset(identifier string) string {
	r := a.readOnlyRegistryCopy()
	entry, exists := r[identifier]
	if !exists {
		return ""
	}
	return entry.Offset
}

// GetTailingMode returns the last committed offset for a given identifier,
// returns an empty string if it does not exist.
func (a *RegistryAuditor) GetTailingMode(identifier string) string {
	r := a.readOnlyRegistryCopy()
	entry, exists := r[identifier]
	if !exists {
		return ""
	}
	return entry.TailingMode
}

// run keeps up to date the registry depending on different events
func (a *RegistryAuditor) run() {
	cleanUpTicker := time.NewTicker(defaultCleanupPeriod)
	flushTicker := time.NewTicker(defaultFlushPeriod)
	defer func() {
		// clean the context
		cleanUpTicker.Stop()
		flushTicker.Stop()
		a.done <- struct{}{}
	}()

	var fileError sync.Once
	for {
		select {
		case msg, isOpen := <-a.inputChan:
			if !isOpen {
				// inputChan has been closed, no need to update the registry anymore
				return
			}
			// update the registry with new entry
			a.updateRegistry(msg.Origin.Identifier, msg.Origin.Offset, msg.Origin.LogSource.Config.TailingMode)
		case <-cleanUpTicker.C:
			// remove expired offsets from registry
			a.cleanupRegistry()
		case <-flushTicker.C:
			// saves current registry into disk
			err := a.flushRegistry()
			if err != nil {
				if os.IsPermission(err) || os.IsNotExist(err) {
					fileError.Do(func() {
						log.Println("W!", err)
					})
				} else {
					log.Println("E!", err)
				}
			}
		}
	}
}

// recoverRegistry rebuilds the registry from the state file found at path
func (a *RegistryAuditor) recoverRegistry() map[string]*RegistryEntry {
	mr, err := ioutil.ReadFile(a.registryPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Println("I! Could not find state file at %q, will start with default offsets", a.registryPath)
		} else {
			log.Println("E!", err)
		}
		return make(map[string]*RegistryEntry)
	}
	r, err := a.unmarshalRegistry(mr)
	if err != nil {
		log.Println("E!", err)
		return make(map[string]*RegistryEntry)
	}
	return r
}

// cleanupRegistry removes expired entries from the registry
func (a *RegistryAuditor) cleanupRegistry() {
	a.registryMutex.Lock()
	defer a.registryMutex.Unlock()
	expireBefore := time.Now().UTC().Add(-a.entryTTL)
	for path, entry := range a.registry {
		if entry.LastUpdated.Before(expireBefore) {
			delete(a.registry, path)
		}
	}
}

// updateRegistry updates the registry entry matching identifier with new the offset and timestamp
func (a *RegistryAuditor) updateRegistry(identifier string, offset string, tailingMode string) {
	a.registryMutex.Lock()
	defer a.registryMutex.Unlock()
	if identifier == "" {
		// An empty Identifier means that we don't want to track down the offset
		// This is useful for origins that don't have offsets (networks), or when we
		// specially want to avoid storing the offset
		return
	}
	a.registry[identifier] = &RegistryEntry{
		LastUpdated: time.Now().UTC(),
		Offset:      offset,
		TailingMode: tailingMode,
	}
}

// readOnlyRegistryCopy returns a read only copy of the registry
func (a *RegistryAuditor) readOnlyRegistryCopy() map[string]RegistryEntry {
	a.registryMutex.Lock()
	defer a.registryMutex.Unlock()
	r := make(map[string]RegistryEntry)
	for path, entry := range a.registry {
		r[path] = *entry
	}
	return r
}

// flushRegistry writes on disk the registry at the given path
func (a *RegistryAuditor) flushRegistry() error {
	r := a.readOnlyRegistryCopy()
	mr, err := a.marshalRegistry(r)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(a.registryPath, mr, 0644)
}

// marshalRegistry marshals a registry
func (a *RegistryAuditor) marshalRegistry(registry map[string]RegistryEntry) ([]byte, error) {
	r := JSONRegistry{
		Version:  registryAPIVersion,
		Registry: registry,
	}
	return json.Marshal(r)
}

// unmarshalRegistry unmarshals a registry
func (a *RegistryAuditor) unmarshalRegistry(b []byte) (map[string]*RegistryEntry, error) {
	var r map[string]interface{}
	err := json.Unmarshal(b, &r)
	if err != nil {
		return nil, err
	}

	return unmarshalRegistry(b)
}
