package operation

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/spec"
)

// loadIndex builds an index over the id-less fixture, which is the interesting
// case: almost nothing in it carries an operationId.
func loadIndex(t *testing.T) *Index {
	t.Helper()

	return NewIndex(loadFixtureOperations(t, "no-operation-ids.yaml"))
}

func loadFixtureOperations(t *testing.T, name string) []Operation {
	t.Helper()

	doc, err := spec.LoadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("LoadFile(%s): %v", name, err)
	}

	return Extract(doc)
}

func TestIndexSynthesisesIdsFromMethodAndPath(t *testing.T) {
	ix := loadIndex(t)

	want := []string{
		"getPets",
		"postPets",
		"getPetsById",
		"deletePetsById",
		"getPetsById2",
		"getStoreOrderItemsByOrderItemId",
		"healthCheck",
		"getWidgetsById",
		"getWidgetsById2",
	}
	if got := ix.IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
}

func TestIndexSynthesisIsStableAcrossLoads(t *testing.T) {
	first := loadIndex(t).IDs()
	second := loadIndex(t).IDs()

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("ids differ between loads: %v then %v", first, second)
	}
}

func TestIndexDisambiguatesCollidingSyntheticIds(t *testing.T) {
	ix := loadIndex(t)

	// /pets/{id} and /pets/by-id both generalise to getPetsById.
	byTemplate, err := ix.Lookup("getPetsById")
	if err != nil {
		t.Fatalf("Lookup(getPetsById): %v", err)
	}
	if byTemplate.Path != "/pets/{id}" {
		t.Fatalf("getPetsById is %s, want the first colliding operation /pets/{id}", byTemplate.Path)
	}

	suffixed, err := ix.Lookup("getPetsById2")
	if err != nil {
		t.Fatalf("Lookup(getPetsById2): %v", err)
	}
	if suffixed.Path != "/pets/by-id" {
		t.Fatalf("getPetsById2 is %s, want /pets/by-id", suffixed.Path)
	}
}

func TestIndexNeverOverwritesAnAuthorsId(t *testing.T) {
	ix := loadIndex(t)

	// GET /widgets claims getWidgetsById, the id GET /widgets/{id} would
	// otherwise synthesise. The author wins and the synthetic one moves aside.
	authored, err := ix.Lookup("getWidgetsById")
	if err != nil {
		t.Fatalf("Lookup(getWidgetsById): %v", err)
	}
	if authored.Path != "/widgets" {
		t.Fatalf("getWidgetsById is %s, want the authored /widgets", authored.Path)
	}

	synthetic, err := ix.Lookup("getWidgetsById2")
	if err != nil {
		t.Fatalf("Lookup(getWidgetsById2): %v", err)
	}
	if synthetic.Path != "/widgets/{id}" {
		t.Fatalf("getWidgetsById2 is %s, want /widgets/{id}", synthetic.Path)
	}
}

func TestIndexLeavesAuthoredIdsAlone(t *testing.T) {
	ix := NewIndex(loadOperations(t))

	want := []string{"listPets", "createPet", "headPets", "optionsPets", "getPet"}
	if got := ix.IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs() = %v, want the spec's own ids %v", got, want)
	}
}

func TestLookupReturnsTheOperation(t *testing.T) {
	ix := loadIndex(t)

	op, err := ix.Lookup("getStoreOrderItemsByOrderItemId")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if op.Method != "GET" || op.Path != "/store/order-items/{orderItemId}" {
		t.Fatalf("Lookup returned %s %s", op.Method, op.Path)
	}
	if op.ID != "getStoreOrderItemsByOrderItemId" {
		t.Fatalf("returned operation carries ID %q, want the synthesised one", op.ID)
	}
}

func TestLookupUnknownIdIsAUsageErrorWithClosestMatches(t *testing.T) {
	ix := loadIndex(t)

	_, err := ix.Lookup("getPetz")
	if err == nil {
		t.Fatal("Lookup of an unknown id returned no error")
	}

	var cerr *clierr.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error %v is not a *clierr.Error", err)
	}
	if cerr.Code != clierr.CodeUsage {
		t.Fatalf("exit code %d, want %d", cerr.Code, clierr.CodeUsage)
	}
	if len(cerr.Alternatives) == 0 {
		t.Fatal("no valid_alternatives on the not-found error")
	}
	if len(cerr.Alternatives) >= len(ix.IDs()) {
		t.Fatalf("alternatives %v is the whole operation list, want only the closest", cerr.Alternatives)
	}
	if cerr.Alternatives[0] != "getPets" {
		t.Fatalf("closest match is %q, want getPets", cerr.Alternatives[0])
	}
}

func TestLookupAlternativesAreCappedAtFive(t *testing.T) {
	ix := loadIndex(t)

	_, err := ix.Lookup("nothingLikeAnyOfThese")

	var cerr *clierr.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error %v is not a *clierr.Error", err)
	}
	if len(cerr.Alternatives) > 5 {
		t.Fatalf("%d alternatives, want at most 5: %v", len(cerr.Alternatives), cerr.Alternatives)
	}
}

func TestLookupIsCaseSensitiveButSuggestsTheCasingFix(t *testing.T) {
	ix := loadIndex(t)

	_, err := ix.Lookup("GETPETS")
	if err == nil {
		t.Fatal("Lookup(GETPETS) matched getPets; lookup must be case-sensitive")
	}

	var cerr *clierr.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error %v is not a *clierr.Error", err)
	}
	if len(cerr.Alternatives) == 0 || cerr.Alternatives[0] != "getPets" {
		t.Fatalf("alternatives %v do not lead with the case-insensitive match getPets", cerr.Alternatives)
	}
}

func TestByTagReturnsOnlyTaggedOperationsInSpecOrder(t *testing.T) {
	ix := loadIndex(t)

	var got []string
	for _, op := range ix.ByTag("pets") {
		got = append(got, op.ID)
	}

	want := []string{"getPets", "postPets", "getPetsById", "deletePetsById", "getPetsById2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ByTag(pets) = %v, want %v", got, want)
	}
}

func TestByTagIsCaseSensitiveAndEmptyForAnUnknownTag(t *testing.T) {
	ix := loadIndex(t)

	if got := ix.ByTag("Pets"); len(got) != 0 {
		t.Fatalf("ByTag(Pets) matched %d operations, want none", len(got))
	}
	if got := ix.ByTag("nosuchtag"); len(got) != 0 {
		t.Fatalf("ByTag(nosuchtag) matched %d operations, want none", len(got))
	}
}

func TestSynthesiseID(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   string
	}{
		{"GET", "/pets/{id}", "getPetsById"},
		{"GET", "/pets", "getPets"},
		{"POST", "/pets", "postPets"},
		{"DELETE", "/store/order-items/{orderItemId}", "deleteStoreOrderItemsByOrderItemId"},
		{"GET", "/", "get"},
		{"GET", "/v1/pet_store/{pet-id}/toys", "getV1PetStoreByPetIdToys"},
	}

	for _, tc := range cases {
		if got := SynthesiseID(tc.method, tc.path); got != tc.want {
			t.Errorf("SynthesiseID(%s, %s) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
