package postgres

import (
	"encoding/json"
	"fmt"
	m "github.com/StuHelper/stuhelper-resources"
)

func (t *transaction) writeDiscovery(d m.DiscoveryDocument) error {
	if d.Revision < 1 || d.At.IsZero() {
		return m.ErrInput
	}
	if !d.Visible {
		d.Title = ""
		d.CourseID = ""
		d.TeacherIDs = nil
	}
	if e := t.save(`INSERT INTO resources.discovery_history(id,revision,data) VALUES($1,$2,$3)`, d, d.Target.ID, d.Revision); e != nil {
		return e
	}
	data, e := json.Marshal(d)
	if e != nil {
		return m.ErrInput
	}
	return t.saveEnvelope(m.Envelope{SpecVersion: "1.0", ID: fmt.Sprintf("discovery:%s:%d", d.Target.ID, d.Revision), Source: "resources", Type: "resources.discovery.v1", Subject: d.Target.ID, Time: d.At, Data: data})
}

// Only the active published version can supply public metadata. A pending
// replacement never contributes its proposed title or associations.
func (t *transaction) saveDiscovery(r m.Record) error {
	d := m.DiscoveryDocument{Target: m.DiscoveryTarget{Source: "resources", ID: r.ID, Kind: "resource"}, Revision: r.Revision, SchoolID: r.SchoolID}
	if e := t.tx.QueryRowContext(t.ctx, `SELECT transaction_timestamp()`).Scan(&d.At); e != nil {
		return safe(e)
	}
	if r.State == "published" && r.ActiveID != "" {
		v, e := t.Version(r.ActiveID)
		if e != nil {
			return e
		}
		d.Visible = v.Version.State == "published" && v.ReferenceID != ""
		d.Title = v.Version.Metadata.Title
		d.CourseID = v.Version.Metadata.Associations.CourseID
		if v.Version.Metadata.Associations.TeacherID != "" {
			d.TeacherIDs = []string{v.Version.Metadata.Associations.TeacherID}
		}
	}
	return t.writeDiscovery(d)
}
func (t *transaction) DiscoveryCurrent(ids []string) ([]m.DiscoveryDocument, error) {
	return list[m.DiscoveryDocument](t, `SELECT h.data FROM resources.records r JOIN resources.versions v ON v.id=r.active_id JOIN resources.discovery_history h ON h.id=r.id AND h.revision=(r.data->>'Revision')::bigint WHERE r.id=ANY($1::text[]) AND r.state='published' AND v.data->'Version'->>'state'='published' AND COALESCE(v.data->>'ReferenceID','')<>'' AND (h.data->>'visible')::boolean ORDER BY r.id COLLATE "C"`, ids)
}
