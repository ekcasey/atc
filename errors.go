package atc

type WorkerNotCreatedError struct {
	CreationError error
}

func (w WorkerNotCreatedError) Error() string {
	return w.CreationError.Error()
}
