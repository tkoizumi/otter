package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The runtime readiness gate requires pilot acceptance to rest on
// INDEPENDENTLY CHECKED DESTINATION OUTCOMES, because "a successful Python exit
// alone does not prove correct business effects". A check has to know where to
// look, so `destination_tables` makes that machine-readable rather than leaving
// it in a migration record.
//
// These pin the two ends of that: the field is carried through parsing, and the
// mistakes worth catching are caught at validate time rather than after a
// deploy.

func destinationManifest(list string) string {
	return "version: 1\nname: castor-sync\nentrypoint: main.py\ndestination_tables:\n" + list
}

func loadDestinationManifest(t *testing.T, body string) (*Manifest, error) {
	t.Helper()
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "main.py"))
	return LoadAndValidate(writeManifest(t, dir, body))
}

func TestManifestCarriesDestinationTables(t *testing.T) {
	m, err := loadDestinationManifest(t, destinationManifest(
		"  - shopify_orders\n  - castor.shopify_orders_archive\n"))
	if err != nil {
		t.Fatalf("a manifest declaring destinations must validate: %v", err)
	}
	got := strings.Join(m.DestinationTables, ",")
	if want := "shopify_orders,castor.shopify_orders_archive"; got != want {
		t.Errorf("destination tables = %q, want %q", got, want)
	}
}

// Omitting the field must stay valid and stay EMPTY: this is an additive
// change, and every manifest written before it exists has to keep working with
// no destinations rather than acquiring a default.
func TestManifestWithoutDestinationTablesIsValidAndEmpty(t *testing.T) {
	m, err := loadDestinationManifest(t,
		"version: 1\nname: castor-sync\nentrypoint: main.py\n")
	if err != nil {
		t.Fatalf("a manifest with no destinations must validate: %v", err)
	}
	if len(m.DestinationTables) != 0 {
		t.Errorf("destination tables = %v, want none", m.DestinationTables)
	}
}

func TestManifestRefusesBadDestinationTables(t *testing.T) {
	cases := []struct {
		name  string
		list  string
		index string
		says  string
	}{
		{"an empty entry", "  - \"\"\n", "destination_tables[0]", "is empty"},
		{"a blank entry", "  - \"   \"\n", "destination_tables[0]", "is empty"},
		{"a duplicate", "  - shopify_orders\n  - shopify_orders\n", "destination_tables[1]", "more than once"},
		{"an embedded space", "  - \"shopify orders\"\n", "destination_tables[0]", "contains whitespace"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadDestinationManifest(t, destinationManifest(tc.list))
			if err == nil {
				t.Fatal("a malformed destination table was accepted")
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("error = %v (%T), want a *ValidationError", err, err)
			}
			if !strings.Contains(err.Error(), tc.index) || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error = %q, want it to name %s and say %q", err.Error(), tc.index, tc.says)
			}
		})
	}
}

// The validation is deliberately looser than the secrets check, because a
// destination is reported rather than acted on. A name with a dash, a dot, a
// dollar or mixed case is a legitimate table name somewhere and must not be
// refused for looking unusual.
func TestManifestAcceptsUnusualButUsableDestinationNames(t *testing.T) {
	m, err := loadDestinationManifest(t, destinationManifest(
		"  - shopify-orders\n  - \"ShopifyOrders\"\n  - db.$table\n  - \"3_leading_digit\"\n"))
	if err != nil {
		t.Fatalf("a usable destination name was refused: %v", err)
	}
	if len(m.DestinationTables) != 4 {
		t.Errorf("destination tables = %v, want all four kept", m.DestinationTables)
	}
}
