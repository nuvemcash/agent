package wire_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nuvemcash/agent/wire"
)

// O contrato de fio é consumido pela api do nuvem.cash — os nomes JSON são estáveis.
func TestSnapshot_JSONGolden(t *testing.T) {
	ts := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	s := wire.Snapshot{
		SchemaVersion: wire.SchemaVersion,
		AgentVersion:  "0.1.0",
		ClusterUID:    "uid-123",
		WindowStart:   ts,
		WindowEnd:     ts.Add(5 * time.Minute),
		Nodes: []wire.Node{{
			Name: "10.0.0.1", ProviderID: "ocid1.instance.oc1..abc",
			Labels:           map[string]string{"pool": "a"},
			CPUCapacityMilli: 4000, CPUAllocatableMilli: 3900,
			MemoryCapacityBytes: 16e9, MemoryAllocatableBytes: 15e9,
		}},
		Usage: []wire.WorkloadUsage{{
			Node: "10.0.0.1", Namespace: "app", WorkloadKind: "Deployment", WorkloadName: "web",
			CPURequestMilliSeconds: 300000, CPUUsageCoreSeconds: 12.5,
			MemoryRequestByteSeconds: 1e12, MemoryWorkingSetByteSeconds: 9e11,
			CoverageSeconds: 300, Labels: map[string]string{"app": "web"},
		}},
		PVCs:          []wire.PVC{{Namespace: "app", Name: "data", VolumeHandle: "ocid1.volume.oc1..v", CapacityBytes: 50e9}},
		LoadBalancers: []wire.LoadBalancer{{Namespace: "app", Name: "web-lb", Ingress: []string{"1.2.3.4"}}},
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"schemaVersion":1`, `"clusterUid":"uid-123"`, `"windowStart":"2026-08-13T12:00:00Z"`,
		`"providerId":"ocid1.instance.oc1..abc"`, `"cpuUsageCoreSeconds":12.5`,
		`"workloadKind":"Deployment"`, `"volumeHandle":"ocid1.volume.oc1..v"`, `"coverageSeconds":300`,
	} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("JSON sem %s:\n%s", want, b)
		}
	}
	var back wire.Snapshot
	if err := json.Unmarshal(b, &back); err != nil || back.Usage[0].CPUUsageCoreSeconds != 12.5 {
		t.Fatalf("roundtrip falhou: %v %+v", err, back.Usage)
	}
	if wire.Path != "/ingest/k8s/v1/snapshots" {
		t.Fatalf("path do contrato mudou: %s", wire.Path)
	}
}
