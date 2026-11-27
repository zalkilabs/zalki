package bus

// UncertainDelivery returns the sentences a client declares for a message to it
// whose delivery is unconfirmed: the one for the receiver, and the one for after
// the receiver ACKed it. Each has one %s for the message id. A kind with no such
// wording, or an unknown kind, gives two empty strings; the caller then keeps
// the generic wording.
func (s *Service) UncertainDelivery(kind string) (receiver, acknowledged string) {
	adapter, err := s.Store.registry.Lookup(kind)
	if err != nil {
		return "", ""
	}
	descriptor := adapter.Descriptor()
	return descriptor.UncertainReceiver, descriptor.UncertainAcknowledged
}
