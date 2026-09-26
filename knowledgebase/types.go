// SPDX-License-Identifier: Apache-2.0

// Package knowledgebase defines provider-neutral records produced by a
// threadpoint project scan.
package knowledgebase

import (
	"encoding/json"
	"sort"
)

// RecordKind identifies the kind of shared knowledgebase record.
type RecordKind string

// RecordKnowledge through RecordPlugin identify the
// canonical record categories produced by a workspace scan.
const (
	RecordKnowledge RecordKind = "knowledge"
	RecordRule      RecordKind = "rule"
	RecordSkill     RecordKind = "skill"
	RecordBridge    RecordKind = "bridge"
	RecordConfig    RecordKind = "config"
	RecordPlugin    RecordKind = "plugin"
)

// Source records where a knowledgebase record came from.
type Source struct {
	Provider string `json:"provider"`
	Path     string `json:"path"`
	Native   bool   `json:"native"`
}

// Record is one provider-neutral shared knowledgebase entry.
type Record struct {
	ID       string            `json:"id"`
	Kind     RecordKind        `json:"kind"`
	Title    string            `json:"title"`
	Summary  string            `json:"summary,omitempty"`
	Sources  []Source          `json:"sources"`
	Paths    []string          `json:"paths,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Catalog is the provider-neutral scan result for a workspace.
type Catalog struct {
	Root     string   `json:"root"`
	Records  []Record `json:"records"`
	Warnings []string `json:"warnings,omitempty"`
}

// Add appends record to the catalog.
func (catalog *Catalog) Add(record Record) {
	catalog.Records = append(catalog.Records, record)
}

// Sort orders records by ID.
func (catalog *Catalog) Sort() {
	sort.Slice(catalog.Records, func(i, j int) bool {
		return catalog.Records[i].ID < catalog.Records[j].ID
	})
}

// JSON renders the catalog as indented JSON.
func (catalog *Catalog) JSON() ([]byte, error) {
	return json.MarshalIndent(catalog, "", "  ")
}
