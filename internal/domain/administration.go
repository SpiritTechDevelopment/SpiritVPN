package domain

// AdministrativeCommand — блокировка/разблокировка в общем потоке команд.
type AdministrativeCommand struct {
	CustomerID    string
	Target        CustomerLifecycle
	CommandNumber uint64
}

type DeleteCommand struct {
	CustomerID    string
	CommandNumber uint64
}

func ValidateAdministrativeCommand(cmd AdministrativeCommand) error {
	if err := ValidateCustomerID(cmd.CustomerID); err != nil {
		return err
	}
	if cmd.CommandNumber == 0 {
		return ErrCommandNumberInvalid
	}
	if cmd.Target != CustomerLifecycleActive && cmd.Target != CustomerLifecycleBlocked {
		return ErrAdministrativeStateInvalid
	}
	return nil
}

func ValidateDeleteCommand(cmd DeleteCommand) error {
	if err := ValidateCustomerID(cmd.CustomerID); err != nil {
		return err
	}
	if cmd.CommandNumber == 0 {
		return ErrCommandNumberInvalid
	}
	return nil
}

type CustomerDeletionState string

const (
	CustomerDeletionPending   CustomerDeletionState = "PENDING"
	CustomerDeletionCompleted CustomerDeletionState = "COMPLETED"
)
