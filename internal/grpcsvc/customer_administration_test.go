package grpcsvc

import (
	"context"
	"testing"

	"github.com/RomanRyabinkin/SpiritVPN/internal/app"
	"github.com/RomanRyabinkin/SpiritVPN/internal/domain"
	customerv1 "github.com/RomanRyabinkin/SpiritVPN/internal/gen/spiritvpn/customer/v1"
)

type stateUseCaseStub struct {
	command app.SetCustomerAccessStateCommand
	err     error
}

func (s *stateUseCaseStub) Execute(_ context.Context, command app.SetCustomerAccessStateCommand) error {
	s.command = command
	return s.err
}

type deleteUseCaseStub struct {
	command app.DeleteCustomerAccessCommand
	state   domain.CustomerDeletionState
	err     error
}

func (s *deleteUseCaseStub) Execute(_ context.Context, command app.DeleteCustomerAccessCommand) (domain.CustomerDeletionState, error) {
	s.command = command
	return s.state, s.err
}

func TestSetCustomerAccessStateMapsRequest(t *testing.T) {
	stateUC := &stateUseCaseStub{}
	srv := NewCustomerAccessServer(nil, nil, nil, CustomerAccessAdministration{State: stateUC})
	_, err := srv.SetCustomerAccessState(context.Background(), &customerv1.SetCustomerAccessStateRequest{
		CustomerId: "customer", State: customerv1.AdministrativeAccessState_ADMINISTRATIVE_ACCESS_STATE_BLOCKED,
		CommandNumber: 9,
	})
	if err != nil {
		t.Fatalf("SetCustomerAccessState: %v", err)
	}
	if stateUC.command.Command.CustomerID != "customer" ||
		stateUC.command.Command.Target != domain.CustomerLifecycleBlocked ||
		stateUC.command.Command.CommandNumber != 9 {
		t.Fatalf("команда %+v", stateUC.command.Command)
	}
}

func TestSetCustomerAccessStateRejectsUnspecified(t *testing.T) {
	srv := NewCustomerAccessServer(nil, nil, nil, CustomerAccessAdministration{State: &stateUseCaseStub{}})
	_, err := srv.SetCustomerAccessState(context.Background(), &customerv1.SetCustomerAccessStateRequest{})
	if stableCode(err) != codeInvalidAdministrativeState {
		t.Fatalf("код = %s, err=%v", stableCode(err), err)
	}
}

func TestDeleteCustomerAccessMapsState(t *testing.T) {
	deleteUC := &deleteUseCaseStub{state: domain.CustomerDeletionCompleted}
	srv := NewCustomerAccessServer(nil, nil, nil, CustomerAccessAdministration{Delete: deleteUC})
	response, err := srv.DeleteCustomerAccess(context.Background(), &customerv1.DeleteCustomerAccessRequest{
		CustomerId: "customer", CommandNumber: 11,
	})
	if err != nil {
		t.Fatalf("DeleteCustomerAccess: %v", err)
	}
	if response.State != customerv1.CustomerDeletionState_CUSTOMER_DELETION_STATE_COMPLETED {
		t.Fatalf("state = %s", response.State)
	}
	if deleteUC.command.Command.CommandNumber != 11 {
		t.Fatalf("команда %+v", deleteUC.command.Command)
	}
}
