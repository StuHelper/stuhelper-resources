package resources

import "context"

const UploadLicenseDocumentID = "upload-license"

// TermsSource resolves the effective published license and its pinned HTTPS URL.
// The host supplies it through the legal module's public read interface.
type TermsSource func(context.Context) (Terms, error)

func (s *Service) currentTerms(ctx context.Context) (Terms, error) {
	if s.deps.TermsSource == nil {
		return s.cfg.Terms, nil
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	terms, err := s.deps.TermsSource(work)
	if err != nil || !validTerms(terms) || terms.DocumentID != UploadLicenseDocumentID {
		return Terms{}, ErrUnavailable
	}
	return terms, nil
}

// bindCurrentConsent returns a copy so persisting the server reference leaves
// the original request and its idempotency fingerprint intact.
func (s *Service) bindCurrentConsent(ctx context.Context, origin Origin) (Origin, error) {
	if origin.Kind != "original" {
		return origin, nil
	}
	if !validConsent(origin.Consent) {
		return Origin{}, ErrInput
	}
	terms, err := s.currentTerms(ctx)
	if err != nil {
		return Origin{}, err
	}
	consent := *origin.Consent
	if consent.Version != terms.Version || consent.Digest != terms.Digest || (consent.DocumentID != "" && consent.DocumentID != terms.DocumentID) {
		return Origin{}, ErrInput
	}
	consent.DocumentID = terms.DocumentID
	origin.Consent = &consent
	return origin, nil
}
