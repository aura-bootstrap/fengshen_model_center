package scoring

import "context"

// Service is the synchronous scoring implementation shared by development
// engines and server adapters. It adds no fallback provider or alternate parser.
type Service struct {
	client *Client
}

func NewService(config Config) (*Service, error) {
	client, err := NewClient(config)
	if err != nil {
		return nil, err
	}
	return &Service{client: client}, nil
}

func NewServiceWithClient(client *Client) (*Service, error) {
	if client == nil {
		return nil, newError(ErrorKindConfig, 0, "", "scoring client is required", nil)
	}
	return &Service{client: client}, nil
}

func (s *Service) ScoreEvents(ctx context.Context, input CandidateInput) ([]*RawEvent, error) {
	return s.client.ScoreEvents(ctx, input)
}

func (s *Service) Score(ctx context.Context, input CandidateInput) (Result, error) {
	return s.client.Score(ctx, input)
}

var _ Scorer = (*Service)(nil)
