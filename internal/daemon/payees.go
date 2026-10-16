package daemon

// payees.go is the payee list of the spending policy (A2A-DESIGN §8.6,
// N-UX1): the AIDs the automatic and the agent tiers may pay. It is always
// on and always payees.allow in the data directory (payeesFile), empty on a
// new node. AdmitSpend and decidePay read it; the routes here edit it the
// way the peer lists are edited (one AID per line, rewritten atomically,
// comments kept), and every change is recorded as anet.policy.changed.
//
// Adding a payee widens what this node can spend, so `anet payees add`
// confirms on the terminal before it calls /payees/add; removing one
// narrows it and needs no confirmation. Both routes are bearer only.

import (
	"net/http"
	"sort"
)

// ListPayees names the payee list for editPeerFile.
const ListPayees = "payees"

// PayeesStatus is the payee list as it stands.
type PayeesStatus struct {
	Path   string   `json:"path"`
	Payees []string `json:"payees"`
	// Error is set when the file cannot be read: the automatic and the
	// agent tiers then pay nobody (payees_unreadable).
	Error string `json:"error,omitempty"`
}

// PayeesStatus reads the payee list.
func (d *Daemon) PayeesStatus() PayeesStatus {
	st := PayeesStatus{Path: d.peerFile(payeesFile), Payees: []string{}}
	set, err := readPeerFile(st.Path)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	for aid := range set {
		st.Payees = append(st.Payees, aid)
	}
	sort.Strings(st.Payees)
	return st
}

// AddPayee puts aid on the payee list.
func (d *Daemon) AddPayee(aid string) (PeerListResult, error) {
	changed, err := d.editPeerFile(ListPayees, aid, true)
	if err != nil {
		return PeerListResult{}, err
	}
	if changed {
		d.recordPolicyChange("payments.payees", nil, aid, nil)
	}
	return PeerListResult{AID: aid, List: ListPayees, Changed: changed}, nil
}

// RemovePayee takes aid off the payee list.
func (d *Daemon) RemovePayee(aid string) (PeerListResult, error) {
	changed, err := d.editPeerFile(ListPayees, aid, false)
	if err != nil {
		return PeerListResult{}, err
	}
	if changed {
		d.recordPolicyChange("payments.payees", aid, nil, nil)
	}
	return PeerListResult{AID: aid, List: ListPayees, Changed: changed}, nil
}

func (d *Daemon) hPayeesList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, d.PayeesStatus())
}

func (d *Daemon) hPayeesAdd(w http.ResponseWriter, r *http.Request) {
	d.payeesEdit(w, r, d.AddPayee)
}

func (d *Daemon) hPayeesRemove(w http.ResponseWriter, r *http.Request) {
	d.payeesEdit(w, r, d.RemovePayee)
}

func (d *Daemon) payeesEdit(w http.ResponseWriter, r *http.Request, edit func(string) (PeerListResult, error)) {
	aid, ok := readAID(w, r)
	if !ok {
		return
	}
	res, err := edit(aid)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
