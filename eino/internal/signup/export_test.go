package signup

// NewReviewReplyTargetDeliveringTo is NewReviewReplyTarget delivering through deliverTo(sessionID) instead of the
// session's Dex RPC target, so a routing test needs no Dex server.
var NewReviewReplyTargetDeliveringTo = newReviewReplyTarget
