// Package payment is a small sample for testing the archaeologist.
// It mimics the shape of a real payment subsystem so we can verify
// that retrieval surfaces the right entities for questions like
// "where is payment handled?".
package payment

import "fmt"

// Provider is anything that can charge a customer for an amount.
// Implementations: StripeProvider, PaypalProvider.
type Provider interface {
	Charge(customerID string, amountCents int) (string, error)
	Refund(txID string) error
}

// StripeProvider charges via the Stripe API.
type StripeProvider struct {
	APIKey string
}

func (s *StripeProvider) Charge(customerID string, amountCents int) (string, error) {
	return fmt.Sprintf("stripe-tx-%s-%d", customerID, amountCents), nil
}

func (s *StripeProvider) Refund(txID string) error {
	return nil
}

// PaypalProvider charges via the PayPal REST API.
type PaypalProvider struct {
	ClientID string
}

func (p *PaypalProvider) Charge(customerID string, amountCents int) (string, error) {
	return fmt.Sprintf("pp-tx-%s-%d", customerID, amountCents), nil
}

func (p *PaypalProvider) Refund(txID string) error {
	return nil
}

// validateAmount returns an error if the amount is not positive.
func validateAmount(amountCents int) error {
	if amountCents <= 0 {
		return fmt.Errorf("amount must be positive, got %d", amountCents)
	}
	return nil
}

// ChargeCustomer is the central entry point for charging a customer.
// HTTP handlers should call this rather than reaching into providers.
func ChargeCustomer(p Provider, customerID string, amountCents int) (string, error) {
	if err := validateAmount(amountCents); err != nil {
		return "", err
	}
	return p.Charge(customerID, amountCents)
}
