package runtime

const warmConversationRootLabel = "io.loopforge.warm-conversation-root"

// WarmDockerConversationID identifies the one sandbox for a private CLI session.
func WarmDockerConversationID(sessionKey string, transient bool) string {
	if transient {
		return "wsl-conversation-transient-" + sessionKey
	}
	return "wsl-conversation-" + sessionKey
}
