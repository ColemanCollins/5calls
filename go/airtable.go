package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fabioberger/airtable-go"
)

const (
	issuesTable    = "Issues list"
	contactsTable  = "Contact"
	additionsTable = "Additions"
	deletionsTable = "Deletions"
)

var minRefreshInterval = time.Minute

// IssueLister is something that can produce a list of all issues.
type IssueLister interface {
	AllIssues() ([]Issue, error)
}

// ContactPatcher is a thing that can produce contact data patches
type ContactPatcher interface {
	AllPatches() ([]Patch, error)
}

func asJson(data interface{}) string {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Sprint(data)
	}
	return string(b)
}

// atPatch is a patch for a civic api contact
type atPatch struct {
	Name  string
	Phone string
	Area  string
	State string
}

// atIssueInfo is the record definition of an issue, minus its key
type atIssueInfo struct {
	Name         string   `json:"Name"`
	Action       string   `json:"Action requested"`
	Script       string   `json:"Script"`
	ContactLinks []string `json:"Contact"`
}

// atIssue is an airtable issue record.
type atIssue struct {
	ID          string `json:"id"`
	atIssueInfo `json:"fields"`
	Contacts    []*atContact `json:"contacts"`
}

func (i *atIssue) String() string { return asJson(i) }

func (i *atIssue) toIssue(contacts []Contact) Issue {
	return Issue{
		ID:       i.ID,
		Name:     i.Name,
		Reason:   i.Action,
		Script:   i.Script,
		Contacts: contacts,
	}
}

// atContactInfo is the record definition of a contact minus its key.
type atContactInfo struct {
	Name     string `json:"Name"`
	Phone    string `json:"Phone"`
	PhotoURL string `json:"PhotoURL"`
	Area     string `json:"Area"`
	Reason   string `json:"Contact Reason"`
}

// atContact is an airtable contact record.
type atContact struct {
	ID            string `json:"id"`
	atContactInfo `json:"fields"`
}

func (c *atContact) String() string { return asJson(c) }

func (c *atContact) toContact() Contact {
	return Contact{
		ID:       c.ID,
		Name:     c.Name,
		Phone:    c.Phone,
		PhotoURL: c.PhotoURL,
		Reason:   c.Reason,
		Area:     c.Area,
	}
}

// AirtableConfig is the configuration for the airtable client.
type AirtableConfig struct {
	BaseID string // ID of the airtable base
	APIKey string // API key for HTTP calls
}

// AirtableClient provides a semantic API to the backend database.
type AirtableClient struct {
	client *airtable.Client
}

func NewAirtableClient(config AirtableConfig) *AirtableClient {
	c, _ := airtable.New(config.APIKey, config.BaseID)
	return &AirtableClient{client: c}
}

// AllPatches returns a list of contact patches
func (c *AirtableClient) AllPatches() ([]Patch, error) {
	// load all additions
	var aList []*atPatch
	err := c.client.ListRecords(additionsTable, &aList, airtable.ListParameters{
		FilterByFormula: `NOT(NAME = "")`,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to load additions, %v", err)
	}

	for _, p := range aList {
		log.Printf("found add %s %s %s", p.Name, p.Phone, p.State)
	}

	// load all deletions
	var dList []*atPatch
	err = c.client.ListRecords(deletionsTable, &dList, airtable.ListParameters{
		FilterByFormula: `NOT(NAME = "")`,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to load deletions, %v", err)
	}

	var patches []Patch
	for _, add := range aList {
		addition := Patch{Name: add.Name, Phone: add.Phone, State: add.State, Area: add.Area, Type: "ADD"}
		patches = append(patches, addition)
	}

	for _, add := range dList {
		deletion := Patch{Name: add.Name, Phone: add.Phone, State: add.State, Area: add.Area, Type: "DELETE"}
		patches = append(patches, deletion)
	}

	return patches, nil
}

// AllIssues returns a list of issues with standard contacts, if any, linked to them.
func (c *AirtableClient) AllIssues() ([]Issue, error) {
	// load all contacts first
	var cList []*atContact
	err := c.client.ListRecords(contactsTable, &cList, airtable.ListParameters{
		FilterByFormula: `NOT(NAME = "")`,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to load contacts, %v", err)
	}
	// index contacts by ID for easy joins
	contactsMap := map[string]*atContact{}
	for _, c := range cList {
		contactsMap[c.ID] = c
	}

	// load all issues
	var list []*atIssue
	err = c.client.ListRecords(issuesTable, &list, airtable.ListParameters{
		FilterByFormula: `NOT(OR(NAME = "", INACTIVE))`,
		Sort: []airtable.SortParameter{
			airtable.SortParameter{
				Field:          "Sort",
				ShouldSortDesc: false,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("unable to load issues, %v", err)
	}
	// normalize and join with contacts
	var ret []Issue
	for _, i := range list {
		var contacts []Contact
		for _, id := range i.ContactLinks {
			contact := contactsMap[id]
			if contact == nil {
				log.Println("[WARN] unable to find contact with ID", id)
				continue
			}
			contacts = append(contacts, contact.toContact())
		}
		ret = append(ret, i.toIssue(contacts))
	}
	return ret, nil
}

// issueCache stores an in-memory copy of the issue list with automatic refresh.
type issueCache struct {
	delegate IssueLister
	stop     chan struct{} // close-only
	force    chan struct{}
	val      atomic.Value // of []Issue
	stopOnce sync.Once
}

// NewIssueCache returns an issue cache after ensuring that the issue list is loaded.
func NewIssueCache(delegate IssueLister, refreshInterval time.Duration) (IssueLister, error) {
	issues, err := delegate.AllIssues()
	if err != nil {
		return nil, err
	}
	if refreshInterval <= minRefreshInterval {
		refreshInterval = minRefreshInterval
	}
	ic := &issueCache{
		delegate: delegate,
		stop:     make(chan struct{}),
		force:    make(chan struct{}, 1),
	}
	ic.val.Store(issues)
	go ic.refresh(refreshInterval)
	return ic, nil
}

// Reload immediately reloads the database in the background.
func (ic *issueCache) Reload() {
	ic.force <- struct{}{}
}

func (ic *issueCache) Close() error {
	ic.stopOnce.Do(func() { close(ic.stop) })
	return nil
}

func (ic *issueCache) refresh(interval time.Duration) {
	reload := func() {
		issues, err := ic.delegate.AllIssues()
		if err != nil {
			log.Println("Error loading issues,", err)
		}
		log.Println(len(issues), "issues loaded")
		ic.val.Store(issues)
	}
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			t.Reset(interval)
			reload()
		case <-ic.force:
			t.Reset(interval)
			reload()
		case <-ic.stop:
			return
		}
	}
}

func (ic *issueCache) AllIssues() ([]Issue, error) {
	return ic.val.Load().([]Issue), nil
}

// patchCache stores an in-memory copy of the issue list with automatic refresh.
type patchCache struct {
	delegate ContactPatcher
	stop     chan struct{} // close-only
	force    chan struct{}
	val      atomic.Value // of []Issue
	stopOnce sync.Once
}

// NewContactPatcher returns an contact patch cache after ensuring that the contact patch list is loaded.
func NewContactPatcher(delegate ContactPatcher, refreshInterval time.Duration) (ContactPatcher, error) {
	patches, err := delegate.AllPatches()
	if err != nil {
		return nil, err
	}
	if refreshInterval <= minRefreshInterval {
		refreshInterval = minRefreshInterval
	}
	pc := &patchCache{
		delegate: delegate,
		stop:     make(chan struct{}),
		force:    make(chan struct{}, 1),
	}
	pc.val.Store(patches)
	go pc.refresh(refreshInterval)
	return pc, nil
}

// Reload immediately reloads the database in the background.
func (pc *patchCache) Reload() {
	pc.force <- struct{}{}
}

func (pc *patchCache) Close() error {
	pc.stopOnce.Do(func() { close(pc.stop) })
	return nil
}

func (pc *patchCache) refresh(interval time.Duration) {
	reload := func() {
		patches, err := pc.delegate.AllPatches()
		if err != nil {
			log.Println("Error loading patches,", err)
		}
		log.Println(len(patches), "patches loaded")
		pc.val.Store(patches)
	}
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			t.Reset(interval)
			reload()
		case <-pc.force:
			t.Reset(interval)
			reload()
		case <-pc.stop:
			return
		}
	}
}

func (pc *patchCache) AllPatches() ([]Patch, error) {
	return pc.val.Load().([]Patch), nil
}
