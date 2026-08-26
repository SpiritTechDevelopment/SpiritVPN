package app

import "context"

// AvailableNode — одна актуальная нода, которую можно показать до покупки.
type AvailableNode struct {
	NodeID      string
	DisplayName string
}

// AvailableFleet — актуальные ноды одного fleet.
type AvailableFleet struct {
	FleetID int64
	Nodes   []AvailableNode
}

// ListAvailableNodes — read-only use case каталога доступных нод.
//
// Здесь нет customer_id и вызовов node-agent: доступность для продажи означает
// только присутствие ноды и её membership в актуальном manifest.
type ListAvailableNodes struct {
	Repo AvailableNodesRepository
}

func NewListAvailableNodes(repo AvailableNodesRepository) *ListAvailableNodes {
	return &ListAvailableNodes{Repo: repo}
}

// Execute возвращает только непустые fleets в стабильном порядке репозитория.
func (uc *ListAvailableNodes) Execute(ctx context.Context) ([]AvailableFleet, error) {
	return uc.Repo.ListAvailableNodes(ctx)
}
