package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
)

type fakeAvailableNodesRepo struct {
	calls  int
	fleets []app.AvailableFleet
	err    error
}

func (r *fakeAvailableNodesRepo) ListAvailableNodes(context.Context) ([]app.AvailableFleet, error) {
	r.calls++
	return r.fleets, r.err
}

func TestListAvailableNodesReturnsRepositorySnapshot(t *testing.T) {
	want := []app.AvailableFleet{{
		FleetID: 10,
		Nodes:   []app.AvailableNode{{NodeID: "NL-1", DisplayName: "Нидерланды"}},
	}}
	repo := &fakeAvailableNodesRepo{fleets: want}

	got, err := app.NewListAvailableNodes(repo).Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if repo.calls != 1 || len(got) != 1 || got[0].FleetID != 10 || got[0].Nodes[0] != want[0].Nodes[0] {
		t.Fatalf("результат %+v, calls=%d", got, repo.calls)
	}
}

func TestListAvailableNodesPropagatesRepositoryError(t *testing.T) {
	want := errors.New("database unavailable")
	repo := &fakeAvailableNodesRepo{err: want}

	_, err := app.NewListAvailableNodes(repo).Execute(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("ошибка %v, ожидалась %v", err, want)
	}
}
