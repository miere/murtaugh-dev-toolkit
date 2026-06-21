package client

import "context"

// fakeAPI is the in-memory SlackAPI used by the package's own tests.
type fakeAPI struct {
	channels []Channel
	users    []User
	dmFor    map[string]string

	listChannelsErr error
	listUsersErr    error
	openDMErr       error

	posted   []PostMessageParams
	updated  []UpdateMessageParams
	uploaded []UploadFileParams

	postResult   PostMessageResult
	postErr      error
	updateResult PostMessageResult
	updateErr    error
	uploadResult PostMessageResult
	uploadErr    error

	history    []Message
	historyErr error
	replies    []Message
	repliesErr error
}

func (f *fakeAPI) PostMessage(_ context.Context, p PostMessageParams) (PostMessageResult, error) {
	f.posted = append(f.posted, p)
	return f.postResult, f.postErr
}

func (f *fakeAPI) UploadFile(_ context.Context, p UploadFileParams) (PostMessageResult, error) {
	f.uploaded = append(f.uploaded, p)
	return f.uploadResult, f.uploadErr
}

func (f *fakeAPI) UpdateMessage(_ context.Context, p UpdateMessageParams) (PostMessageResult, error) {
	f.updated = append(f.updated, p)
	return f.updateResult, f.updateErr
}

func (f *fakeAPI) GetHistory(_ context.Context, _ string, _ string, _ int) ([]Message, error) {
	return f.history, f.historyErr
}

func (f *fakeAPI) GetReplies(_ context.Context, _, _, _ string) ([]Message, error) {
	return f.replies, f.repliesErr
}

func (f *fakeAPI) ListChannels(_ context.Context) ([]Channel, error) {
	return f.channels, f.listChannelsErr
}

func (f *fakeAPI) ListUsers(_ context.Context) ([]User, error) {
	return f.users, f.listUsersErr
}

func (f *fakeAPI) OpenDM(_ context.Context, userID string) (string, error) {
	if f.openDMErr != nil {
		return "", f.openDMErr
	}
	return f.dmFor[userID], nil
}

func (f *fakeAPI) CreateChannel(_ context.Context, _ CreateChannelParams) (CreateChannelResult, error) {
	return CreateChannelResult{}, nil
}
