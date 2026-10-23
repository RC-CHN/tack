package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func spoolFixture(t *testing.T) (*Node, journal) {
	t.Helper()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	rec := journal{Task: p.Task{ID: "result-a", NodeID: a.config.NodeID, Sequence: 1, Command: p.Command{Script: "must not remain in completed state"}}, Report: p.Report{TaskID: "result-a", State: "succeeded", Result: &p.Result{Stdout: bytes.Repeat([]byte("o"), 70001), Stderr: bytes.Repeat([]byte("e"), 81003)}}}
	return a, rec
}

func TestSpoolCrashRecoveryAndChunkResume(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint("pending-", pending), func(t *testing.T) {
			a, rec := spoolFixture(t)
			saved, err := a.spoolRecord(rec)
			if err != nil {
				t.Fatal(err)
			}
			if len(saved.Report.Result.Stdout) != 0 || saved.Task.Command.Script != "" || *saved.Spool != p.OutputManifest(rec.Report.Result) {
				t.Fatal("body retained in metadata")
			}
			if pending {
				if err := os.Rename(a.spoolPath(rec.Task.ID), a.spoolPath(rec.Task.ID)+".tack-pending"); err != nil {
					t.Fatal(err)
				}
			}
			// Crash before journal completion: only the execution intent survived.
			intent := rec
			intent.Report = p.Report{TaskID: rec.Task.ID, State: "running"}
			records := map[string]journal{rec.Task.ID: intent}
			if err := a.recoverSpools(records); err != nil {
				t.Fatal(err)
			}
			got := records[rec.Task.ID]
			if got.Report.State != "succeeded" || got.Spool == nil {
				t.Fatal("lost durable completion")
			}
			all := append(append([]byte{}, rec.Report.Result.Stdout...), rec.Report.Result.Stderr...)
			offset := 123 // Resume an arbitrary pre-crash upload offset.
			a.uploadClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				var in p.ResultChunk
				if err := p.Decode(r.Body, &in); err != nil {
					return nil, err
				}
				if !in.Probe {
					if in.Offset != offset || len(in.Data) > p.MaxChunk || !bytes.Equal(in.Data, all[offset:offset+len(in.Data)]) {
						t.Error("incorrect resumed chunk")
					}
					offset += len(in.Data)
				}
				return response(p.ChunkAck{Version: p.TransferVersion, TaskID: in.TaskID, Offset: offset, Complete: offset == len(all)}), nil
			})}
			if err := a.upload(context.Background(), got); err != nil {
				t.Fatal(err)
			}
			if offset != len(all) {
				t.Fatal("missing uploaded bytes")
			}
			m := a.nodeMetrics(records, "")
			if m.PendingResultBytes != len(all) || m.PendingResults != 1 {
				t.Fatal("disk-backed output escaped admission accounting")
			}
			if _, err := os.Stat(a.spoolPath(rec.Task.ID) + ".tack-pending"); !os.IsNotExist(err) {
				t.Fatal("pending file survived recovery")
			}
		})
	}
}

func TestSpoolAcknowledgementDurabilityBeforeDelete(t *testing.T) {
	a, rec := spoolFixture(t)
	path := filepath.Join(a.config.StateDir, "journal.json")
	records := map[string]journal{rec.Task.ID: rec}
	if err := a.persistJournal(path, records, true); err != nil {
		t.Fatal(err)
	}
	ack := acknowledge(records[rec.Task.ID])
	records[rec.Task.ID] = ack
	write := a.writeJSON
	a.writeJSON = func(string, any) error { return syscall.ENOSPC }
	if err := a.persistJournal(path, records, true); err == nil {
		t.Fatal("expected failed receipt commit")
	}
	if _, err := os.Stat(a.spoolPath(rec.Task.ID)); err != nil {
		t.Fatal("discarded output before receipt was durable")
	}
	disk, _, err := readJournal(path)
	if err != nil || disk[rec.Task.ID].Acknowledged {
		t.Fatal("failed receipt appeared committed", err)
	}
	a.writeJSON = write
	// Simulate a stop just AFTER saving the receipt and BEFORE removing output.
	if err := write(path, retainedJournal{StorageVersion: 4, Records: records}); err != nil {
		t.Fatal(err)
	}
	restarted, _, err := readJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.recoverSpools(restarted); err != nil {
		t.Fatal(err)
	}
	if err := a.persistJournal(path, restarted, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.spoolPath(rec.Task.ID)); !os.IsNotExist(err) {
		t.Fatal("acknowledged output not reclaimed")
	}
	// A second crash after removal but before persisting the cleared pointer is safe.
	if err := os.Remove(a.spoolDir()); err != nil {
		t.Fatal(err)
	}
	restarted, _, err = readJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.recoverSpools(restarted); err != nil {
		t.Fatal(err)
	}
	if err := a.persistJournal(path, restarted, true); err != nil {
		t.Fatal(err)
	}
}

func TestSpoolCorruptionOrphanAndMissingBodyFailClosed(t *testing.T) {
	for _, mode := range []string{"corrupt", "orphan", "missing", "identity", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			a, rec := spoolFixture(t)
			saved, err := a.spoolRecord(rec)
			if err != nil {
				t.Fatal(err)
			}
			path := a.spoolPath(rec.Task.ID)
			records := map[string]journal{rec.Task.ID: saved}
			switch mode {
			case "corrupt":
				f, err := os.OpenFile(path, os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				info, _ := f.Stat()
				_, err = f.WriteAt([]byte("!"), info.Size()-1)
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "orphan":
				delete(records, rec.Task.ID)
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "identity":
				saved.Task.Sequence++
				records[rec.Task.ID] = saved
			case "conflict":
				saved.Report.State = "failed"
				records[rec.Task.ID] = saved
			}
			if err := a.recoverSpools(records); err == nil {
				t.Fatal("invalid result accepted")
			}
			if mode != "missing" {
				if _, err := os.Stat(path); err != nil {
					t.Fatal("discarded recovery evidence")
				}
			}
		})
	}
}

func TestSpoolInterruptedMigrationAndBoundedJournal(t *testing.T) {
	a, rec := spoolFixture(t)
	path := filepath.Join(a.config.StateDir, "journal.json")
	legacy := map[string]journal{}
	for i := 0; i < 8; i++ {
		item := rec
		item.Task.ID = fmt.Sprintf("result-%02d", i)
		item.Task.Sequence = uint64(i + 1)
		item.Report.TaskID = item.Task.ID
		legacy[item.Task.ID] = item
	}
	if err := p.WriteJSON(path, retainedJournal{StorageVersion: 3, Records: legacy}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	converted := 0
	_, _, err := visitJournal(path, func(rec journal) (journal, error) {
		converted++
		if converted == 4 {
			return rec, syscall.ENOSPC
		}
		return a.spoolRecord(rec)
	})
	if err == nil {
		t.Fatal("expected interrupted migration")
	}
	unchanged, _ := os.ReadFile(path)
	if !bytes.Equal(before, unchanged) {
		t.Fatal("source overwritten during failed migration")
	}
	// A partial spool from the interrupted next write must not prevent retry.
	if err := os.WriteFile(a.spoolPath("result-03")+".tack-pending", []byte(spoolMagic), 0600); err != nil {
		t.Fatal(err)
	}
	records, retained, err := visitJournal(path, a.spoolRecord)
	if err != nil || !retained || len(records) != 8 {
		t.Fatal("migration retry failed", err)
	}
	if err := a.recoverSpools(records); err != nil {
		t.Fatal(err)
	}
	if err := a.persistJournal(path, records, true); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Size() > 16*1024 {
		t.Fatal("result bodies still embedded in journal")
	}
	data, _ := os.ReadFile(path)
	var old struct {
		StorageVersion int `json:"storage_version"`
	}
	json.Unmarshal(data, &old)
	if old.StorageVersion != 4 {
		t.Fatal("old reader can mistake new storage for v3")
	}
	mod := map[string]time.Time{}
	for id := range records {
		info, _ := os.Stat(a.spoolPath(id))
		mod[id] = info.ModTime()
	}
	for i := 0; i < 10; i++ {
		if err := a.persistJournal(path, records, true); err != nil {
			t.Fatal(err)
		}
	}
	for id, when := range mod {
		info, _ := os.Stat(a.spoolPath(id))
		if !info.ModTime().Equal(when) {
			t.Fatal("unchanged result rewritten on journal transition")
		}
		file, _, _, err := readSpool(a.spoolPath(id), true)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(file)
		file.Close()
		if err != nil || !bytes.Equal(body, append(append([]byte{}, rec.Report.Result.Stdout...), rec.Report.Result.Stderr...)) {
			t.Fatal("migration changed output")
		}
	}
}

func TestSpoolUploadRetryReopensRepairedFile(t *testing.T) {
	a, rec := spoolFixture(t)
	saved, err := a.spoolRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	path := a.spoolPath(rec.Task.ID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "repaired.out")
	if err := p.WriteFile(replacement, data); err != nil {
		t.Fatal(err)
	}
	repaired := false
	a.Logf = func(string, ...any) {
		if repaired {
			return
		}
		// This must succeed on Windows too: upload must close its file BEFORE
		// reporting the error and waiting, not retain it until process shutdown.
		if err := p.ReplaceFile(replacement, path); err != nil {
			t.Error(err)
		}
		repaired = true
	}
	probes, offset := 0, 0
	all := append(append([]byte{}, rec.Report.Result.Stdout...), rec.Report.Result.Stderr...)
	a.uploadClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		var in p.ResultChunk
		if err := p.Decode(r.Body, &in); err != nil {
			return nil, err
		}
		if in.Probe {
			probes++
			if probes == 1 {
				if err := os.Truncate(path, 0); err != nil {
					t.Error(err)
				}
				out := response(map[string]string{"error": "injected interruption"})
				out.StatusCode = 503
				return out, nil
			}
		} else {
			if in.Offset != offset || !bytes.Equal(in.Data, all[offset:offset+len(in.Data)]) {
				t.Error("repaired output mismatch")
			}
			offset += len(in.Data)
		}
		return response(p.ChunkAck{Version: p.TransferVersion, TaskID: in.TaskID, Offset: offset, Complete: offset == len(all)}), nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.upload(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if probes != 2 || !repaired || offset != len(all) {
		t.Fatal("did not resume after repair")
	}
}
