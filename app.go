package corefactorer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/antchfx/htmlquery"

	"github.com/google/go-github/v64/github"
	"github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"
	"github.com/yuin/goldmark"
)

type App struct {
	openAIClient *openai.Client
	githubClient *github.Client
	httpClient   *http.Client
}

func New(openAIClient *openai.Client, githubClient *github.Client, httpClient *http.Client) *App {
	return &App{
		openAIClient: openAIClient,
		githubClient: githubClient,
		httpClient:   httpClient,
	}
}

// CreateRefactoringTarget creates `RefactoringTarget` from the given prompt with OpenAI FunctionCalling feature
func (a *App) CreateRefactoringTarget(ctx context.Context, prompt string) (*RefactoringTarget, error) {
	resp, err := a.openAIClient.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model: openai.GPT4oMini,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleUser,
					Content: prompt,
				},
			},
			Tools: []openai.Tool{
				{
					Type: openai.ToolTypeFunction,
					Function: &openai.FunctionDefinition{
						Name: "extractRefactoringTarget",
						Parameters: &jsonschema.Definition{
							Type: jsonschema.Object,
							Properties: map[string]jsonschema.Definition{
								"pullRequestUrls": {
									Type:        jsonschema.Array,
									Description: "Pull-request URLs in GitHub to refer to for refactoring",
									Items: &jsonschema.Definition{
										Type: jsonschema.String,
									},
								},
								"files": {
									Type:        jsonschema.Array,
									Description: "List of target files to be refactored",
									Items: &jsonschema.Definition{
										Type: jsonschema.String,
									},
								},
							},
							Required: []string{"pullRequestUrls", "files"},
						},
					},
				},
			},
		},
	)
	if err != nil {
		return nil, err
	}

	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}
	toolCalls := resp.Choices[0].Message.ToolCalls
	if len(toolCalls) == 0 {
		return nil, fmt.Errorf("no tool_calls in response")
	}

	target := &RefactoringTarget{
		UserPrompt: prompt,
		ToolCallID: toolCalls[0].ID,
	}
	for _, toolCall := range toolCalls {
		var tmp RefactoringTarget
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &tmp); err != nil {
			return nil, fmt.Errorf("failed to json.Unmarshal: %w", err)
		}
		target.PullRequestURLs = append(target.PullRequestURLs, tmp.PullRequestURLs...)
		target.Files = append(target.Files, tmp.Files...)
	}

	return target.Unique(), nil
}

// CreateRefactoringRequest creates `RefactoringRequest`.
// It fetches pull request content from GitHub and file content local machine.
func (a *App) CreateRefactoringRequest(ctx context.Context, target *RefactoringTarget) (*RefactoringRequest, error) {
	request := &RefactoringRequest{
		ToolCallID: target.ToolCallID,
		UserPrompt: target.UserPrompt,
	}
	for _, prURL := range target.PullRequestURLs {
		owner, repo, number, err := parsePullRequestURL(prURL)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pull-request url '%s': %w", prURL, err)
		}
		pr, _, err := a.githubClient.PullRequests.Get(ctx, owner, repo, int(number))
		if err != nil {
			return nil, fmt.Errorf("failed to get pull-request content '%s': %w", prURL, err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pr.GetURL(), nil)
		if err != nil {
			return nil, fmt.Errorf("failed to NewRequestWithContext: %w", err)
		}
		req.Header.Add("Accept", "application/vnd.github.diff")
		resp, err := a.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to Do HTTP request: %w", err)
		}
		defer resp.Body.Close()
		diff, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response body: %w", err)
		}

		request.PullRequests = append(request.PullRequests, &PullRequest{
			URL:  prURL,
			Diff: string(diff),
			// Title and Body are not used yet, maybe use them in the future.
			Title: pr.GetTitle(),
			Body:  pr.GetBody(),
		})
	}

	for _, f := range target.Files {
		file, err := os.Open(f)
		if err != nil {
			return nil, fmt.Errorf("failed to open file '%s': %w", f, err)
		}
		content, err := io.ReadAll(file)
		if err != nil {
			return nil, fmt.Errorf("failed to read file content '%s': %w", f, err)
		}
		request.TargetFiles = append(request.TargetFiles, &TargetFile{
			Path:    f,
			Content: string(content),
		})
	}

	return request, nil
}

// CreateRefactoringResult sends a request of refactoring to OpenAI API.
// The chat message in the request includes an original user prompt and fetched pull-request info and file content in given `RefactoringRequest`.
func (a *App) CreateRefactoringResult(ctx context.Context, req *RefactoringRequest) (*RefactoringResult, error) {
	// TODO: https://platform.openai.com/docs/guides/function-calling
	// Preserve first result message
	// 1. Original assistanceMessage
	// 2. Preserved first result message
	// 3. PR info and file content
	assistanceMessage, err := req.CreateAssistanceMessage()
	if err != nil {
		return nil, fmt.Errorf("failed to create assistance message: %w", err)
	}
	// fmt.Printf("--- assistanceMessage ---\n%s", assistanceMessage)

	messages := make([]openai.ChatCompletionMessage, 0, 5)
	messages = append(messages, []openai.ChatCompletionMessage{
		{
			Role:    openai.ChatMessageRoleUser,
			Content: req.UserPrompt,
		},
		{
			Role:    openai.ChatMessageRoleAssistant,
			Content: assistanceMessage,
		},
	}...)

	resp, err := a.openAIClient.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model:    openai.GPT4oMini,
			Messages: messages,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create chat completion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	return &RefactoringResult{
		RawContent: resp.Choices[0].Message.Content,
	}, nil
}

func (a *App) ApplyRefactoringResult(ctx context.Context, result *RefactoringResult) error {
	//markdown := goldmark.New()
	//doc := markdown.Parser().Parse(text.NewReader([]byte(result.RawContent)))
	//doc.FirstChild()
	var out strings.Builder
	if err := goldmark.Convert([]byte(result.RawContent), &out); err != nil {
		return err
	}
	fmt.Printf("--- after convert ---\n%s", out.String())

	doc, err := htmlquery.Parse(strings.NewReader(out.String()))
	if err != nil {
		return err
	}
	headings := htmlquery.Find(doc, "//h3/text()")
	for _, h := range headings {
		fmt.Printf("Path: %s\n", h.Data)
	}

	codes := htmlquery.Find(doc, "//h3/following-sibling::p[1]/code/text()")
	for _, c := range codes {
		fmt.Printf("--- code ---\n%s\n", c.Data)
	}

	return nil
}

func (a *App) dumpOpenAIResponse(resp *openai.ChatCompletionResponse) { //nolint:unused
	fmt.Printf("Choices:\n")
	for i, choice := range resp.Choices {
		fmt.Printf("  %d. Text: %s\n", i, choice.Message.Content)
		fmt.Printf("     ToolCalls:\n")
		for j, toolCall := range choice.Message.ToolCalls {
			fmt.Printf("       %d. FunctionName: %s\n", j, toolCall.Function.Name)
			fmt.Printf("           Arguments: %s\n", toolCall.Function.Arguments)
		}
	}
}
