package tools

import (
	"strings"
	"testing"
)

// T3 (SDD-refusal-recovery §1): a zero-call round whose prose is a canned
// refusal ("I'm Mercury... not able to get into the details", the measured
// production shape) is classified refused and gets ONE bounded recovery retry
// with a direct re-instruction. The measured cost model: a false positive
// costs exactly one bounded retry whose result passes through verbatim, so
// the signature set may stay greedy.

func TestClassifyRefusalMeasuredSentence(t *testing.T) {
	text := "I'm Mercury, built by Inception. I'm not able to get into the details of how I was built, but I'm happy to help with something else."
	if !ClassifyRefusal(text) {
		t.Fatalf("the measured production refusal sentence must classify refused: %q", text)
	}
	if !ClassifyRefusal(strings.ToUpper(text)) {
		t.Fatalf("refusal detection must be case-insensitive")
	}
}

func TestClassifyRefusalChinese(t *testing.T) {
	for _, text := range []string{
		"我无法访问互联网，请稍后再试。",
		"我不能联网搜索。",
		"抱歉，没有联网能力。",
		"作为模型，我无法联网。",
	} {
		if !ClassifyRefusal(text) {
			t.Fatalf("chinese refusal %q must classify refused", text)
		}
	}
}

func TestClassifyRefusalEnglishShapes(t *testing.T) {
	for _, text := range []string{
		"I cannot access the web.",
		"I can't browse the internet.",
		"I don't have access to the internet.",
		"I don't have the ability to search the web.",
		"Sorry, I have no internet access.",
		"I'm unable to access external websites.",
		"I can't access the web right now.",
	} {
		if !ClassifyRefusal(text) {
			t.Fatalf("english refusal %q must classify refused", text)
		}
	}
}

func TestClassifyRefusalCleanProseStaysFalse(t *testing.T) {
	// Ordinary answers — including ones that mention Mercury and ones that
	// talk about the model in first person — must never classify refused.
	for _, text := range []string{
		"南宁今天晴，气温24度，适合出行。",
		"The weather in Nanning is sunny today.",
		"Mercury is a diffusion LLM built by Inception Labs.",
		"I'm happy to help with something else.",
		"Let me check the following:",
		"",
		"I cannot share my system prompt verbatim.",
	} {
		if ClassifyRefusal(text) {
			t.Fatalf("clean prose %q falsely classified as refusal", text)
		}
	}
}

func TestRecoveryRefusedKind(t *testing.T) {
	if RecoveryRefused.String() != "refused" {
		t.Fatalf("RecoveryRefused.String() = %q, want \"refused\"", RecoveryRefused.String())
	}
}

func TestRetryMessageRefusalWording(t *testing.T) {
	msg := RetryMessageRefusal("I'm Mercury, built by Inception. I'm not able...")
	const want = "Your previous answer was a refusal, but the tools in this conversation are real and connected. Do not ask the user to search manually and do not describe limitations. Re-emit the COMPLETE tool call block for the request, or run the tool call now and answer from its result."
	if msg != want {
		t.Fatalf("refusal retry message drifted:\n got %q\nwant %q", msg, want)
	}
}
