# Pollytool API Documentation

This document describes how to use the pollytool package as a library to interact with various LLM providers.

## Installation

```bash
go get github.com/pkdindustries/pollytool
```

## Core Interfaces

### LLM Interface

The main interface for interacting with language models:

```go
import "github.com/pkdindustries/pollytool/llm"

type LLM interface {
    ChatCompletionStream(
        ctx context.Context,
        req *CompletionRequest,
        processor StreamProcessor,
    ) <-chan *messages.StreamEvent
}
```

### CompletionRequest

Configuration for LLM requests:

```go
type CompletionRequest struct {
    Model          string                   // Model identifier (e.g., "gpt-4.1", "claude-opus-4-1-20250805
    Messages       []messages.ChatMessage   // Conversation history
    Temperature    float32                  // Sampling temperature (0.0-1.0)
    MaxTokens      int                      // Maximum tokens to generate
    Tools          []tools.Tool             // Available tools for function calling
    ResponseSchema *Schema                  // JSON schema for structured output
    Timeout        time.Duration            // Request timeout
    BaseURL        string                   // Custom API endpoint (for OpenAI-compatible)
}
```

## Helper Functions

The `llm/helpers` package provides convenience functions to simplify common LLM operations. These functions automatically handle client creation using API keys from environment variables:

### Simplest: One-line Completions

```go
import "github.com/pkdindustries/pollytool/llm"

// Quick completion - just pass the model and prompt
// API keys are loaded from POLLYTOOL_*KEY environment variables
response, err := llm.QuickComplete(ctx, "openai/gpt-4.1", "Tell me a joke", 1000)

// Or with more tokens for longer output
response, err := llm.QuickComplete(ctx, "anthropic/claude-opus-4-1-20250805", "Write a story", 4000)
```

### Streaming with Callback

```go
// Stream completion with real-time output
err := llm.StreamComplete(ctx, "openai/gpt-4.1", "Write a story", 2000, func(chunk string) {
    fmt.Print(chunk) // Print each chunk as it arrives
})

// Or with different token limit
err := llm.StreamComplete(ctx, "gemini/gemini-2.0-flash", "Explain quantum physics", 3000, func(chunk string) {
    fmt.Print(chunk)
})
```

### Conversation with History

```go
// Maintain conversation context
history := []messages.ChatMessage{
    {Role: messages.MessageRoleSystem, Content: "You are helpful"},
    {Role: messages.MessageRoleUser, Content: "Hi"},
    {Role: messages.MessageRoleAssistant, Content: "Hello! How can I help?"},
}

response, err := llm.ChatWithHistory(ctx, "openai/gpt-4.1", history, "What did I just say?", 1000)
```

### Structured JSON Output

```go
// Define your result struct
type UserInfo struct {
    Name  string `json:"name"`
    Age   int    `json:"age"`
    Email string `json:"email"`
}

// Define schema
schema := &llm.Schema{
    Raw: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "name":  map[string]any{"type": "string"},
            "age":   map[string]any{"type": "integer"},
            "email": map[string]any{"type": "string"},
        },
        "required": []string{"name", "email"},
    },
}

// Get structured response
var user UserInfo
err := llm.StructuredComplete(ctx, "openai/gpt-4.1", 
    "Extract: John Doe, 30, john@example.com", schema, 500, &user)
// user now contains: {Name: "John Doe", Age: 30, Email: "john@example.com"}
```

### Builder Pattern for Complex Requests

```go
// The builder still requires a client for advanced use cases
client := llm.NewMultiPass(apiKeys) // Or use getDefaultClient() if you add an export

// Fluent API for building requests
result, err := llm.NewCompletionBuilder("openai/gpt-4.1").
    WithSystemPrompt("You are a helpful assistant").
    WithUserMessage("Tell me about Go").
    WithTemperature(0.8).
    WithMaxTokens(500).
    Execute(ctx, client)

// Or with streaming
err := llm.NewCompletionBuilder("openai/gpt-4.1").
    WithSystemPrompt("You are a creative writer").
    WithUserMessage("Write a haiku").
    ExecuteStreaming(ctx, client, func(chunk string) {
        fmt.Print(chunk)
    })
```

### Automatic Tool Handling

```go
// Setup tools
weatherTool := &WeatherTool{}
registry := tools.NewToolRegistry([]tools.Tool{weatherTool})

// Execute with automatic tool handling
response, err := llm.NewCompletionBuilder("openai/gpt-4.1").
    WithUserMessage("What's the weather in NYC?").
    WithTools(registry.All()).
    ExecuteWithTools(ctx, client, registry)
// Automatically calls weather tool and returns final response
```

### Before vs After Comparison

**Before (using raw API):**
```go
// First, set up API keys
apiKeys := map[string]string{
    "openai": os.Getenv("POLLYTOOL_OPENAIKEY"),
}
client := llm.NewMultiPass(apiKeys)

// Then create and execute request
req := &llm.CompletionRequest{
    Model: "openai/gpt-4.1",
    Messages: []messages.ChatMessage{
        {Role: messages.MessageRoleUser, Content: "Tell me a joke"},
    },
    Temperature: 0.7,
    MaxTokens: 2000,
    Timeout: 30 * time.Second,
}
processor := messages.NewStreamProcessor()
eventChan := client.ChatCompletionStream(ctx, req, processor)
var result string
for event := range eventChan {
    switch event.Type {
    case messages.EventTypeContent:
        result += event.Content
    case messages.EventTypeError:
        return event.Error
    }
}
```

**After (using helpers):**
```go
// Just one line - API keys loaded from environment automatically
result, err := llm.QuickComplete(ctx, "openai/gpt-4.1", "Tell me a joke", 1000)
```

## Provider Support

### MultiPass Provider

The recommended way to use multiple providers
```go
import (
    "github.com/pkdindustries/pollytool/llm"
    "github.com/pkdindustries/pollytool/messages"
)

// Create MultiPass with API keys
apiKeys := map[string]string{
    "openai":    os.Getenv("POLLYTOOL_OPENAIKEY"),
    "anthropic": os.Getenv("POLLYTOOL_ANTHROPICKEY"),
    "gemini":    os.Getenv("POLLYTOOL_GEMINIKEY"),
    "ollama":    os.Getenv("POLLYTOOL_OLLAMAKEY"),
}

multipass := llm.NewMultiPass(apiKeys)

// Create request
req := &llm.CompletionRequest{
    Model: "anthropic/claude-opus-4-1-20250805",
    Messages: []messages.ChatMessage{
        {
            Role:    messages.MessageRoleSystem,
            Content: "You are a helpful assistant.",
        },
        {
            Role:    messages.MessageRoleUser,
            Content: "Hello, how are you?",
        },
    },
    Temperature: 0.7,
    MaxTokens:   1000,
    Timeout:     30 * time.Second,
}

// Stream response
processor := messages.NewStreamProcessor()
eventChan := multipass.ChatCompletionStream(ctx, req, processor)

for event := range eventChan {
    switch event.Type {
    case messages.EventTypeContent:
        fmt.Print(event.Content)
    case messages.EventTypeComplete:
        fmt.Printf("\nComplete: %+v\n", event.Message)
    case messages.EventTypeError:
        fmt.Printf("Error: %v\n", event.Error)
    }
}
```

### Direct Provider Usage

You can also use providers directly:

```go
// OpenAI
client := llm.NewOpenAIClient(apiKey)

// Anthropic  
client := llm.NewAnthropicClient(apiKey)

// Gemini
client := llm.NewGeminiClient(apiKey)

// Ollama (local)
client := llm.NewOllamaClient(apiKey) // apiKey can be empty for local
```

## Message Types

### ChatMessage

```go
type ChatMessage struct {
    Role       MessageRole              // "system", "user", "assistant", or "tool"
    Content    string                   // Text content
    ToolCalls  []ChatMessageToolCall    // Tool/function calls from assistant
    ToolCallID string                   // ID for tool response messages
}

type ChatMessageToolCall struct {
    ID        string   // Unique identifier for this tool call
    Name      string   // Name of the tool to call
    Arguments string   // JSON string of arguments
}
```

### Message Roles

```go
const (
    MessageRoleSystem    MessageRole = "system"
    MessageRoleUser      MessageRole = "user"
    MessageRoleAssistant MessageRole = "assistant"
    MessageRoleTool      MessageRole = "tool"
)
```

## Streaming Events

The streaming API returns events through a channel:

```go
type StreamEvent struct {
    Type    EventType
    Content string           // For EventTypeContent
    Message *ChatMessage     // For EventTypeComplete
    Error   error           // For EventTypeError
}

type EventType string

const (
    EventTypeContent  EventType = "content"   // Streaming text chunk
    EventTypeToolCall EventType = "tool_call" // Tool call in progress
    EventTypeComplete EventType = "complete"  // Final message with all content
    EventTypeError    EventType = "error"     // Error occurred
)
```

## Tool Integration

### Defining Tools

Tools allow LLMs to call functions. Implement the Tool interface:

```go
import (
    "github.com/pkdindustries/pollytool/tools"
    "github.com/modelcontextprotocol/go-sdk/jsonschema"
)

type Tool interface {
    GetSchema() *jsonschema.Schema
    Execute(ctx context.Context, args map[string]any) (string, error)
}
```

### Example Tool Implementation

```go
type WeatherTool struct{}

func (w *WeatherTool) GetSchema() *jsonschema.Schema {
    return &jsonschema.Schema{
        Title:       "get_weather",
        Description: "Get the current weather for a location",
        Type:        "object",
        Properties: map[string]*jsonschema.Schema{
            "location": {
                Type:        "string",
                Description: "The city and state, e.g. San Francisco, CA",
            },
        },
        Required: []string{"location"},
    }
}

func (w *WeatherTool) Execute(ctx context.Context, args map[string]any) (string, error) {
    location, ok := args["location"].(string)
    if !ok {
        return "", fmt.Errorf("location is required")
    }
    
    // Implementation here
    return fmt.Sprintf("The weather in %s is sunny and 72°F", location), nil
}
```

### Using Tools with LLM

```go
// Create tool registry
registry := tools.NewToolRegistry([]tools.Tool{
    &WeatherTool{},
})

// Include tools in request
req := &llm.CompletionRequest{
    Model: "openai/gpt-4.1",
    Messages: []messages.ChatMessage{
        {
            Role:    messages.MessageRoleUser,
            Content: "What's the weather in San Francisco?",
        },
    },
    Tools: registry.All(),
}

// Process response with tool calls
eventChan := client.ChatCompletionStream(ctx, req, processor)

for event := range eventChan {
    if event.Type == messages.EventTypeComplete && len(event.Message.ToolCalls) > 0 {
        // Execute tool calls
        for _, toolCall := range event.Message.ToolCalls {
            var args map[string]any
            json.Unmarshal([]byte(toolCall.Arguments), &args)
            
            tool, _ := registry.Get(toolCall.Name)
            result, err := tool.Execute(ctx, args)
            
            // Add tool result to conversation
            messages = append(messages, messages.ChatMessage{
                Role:       messages.MessageRoleTool,
                Content:    result,
                ToolCallID: toolCall.ID,
            })
        }
        
        // Continue conversation with tool results
        req.Messages = messages
        eventChan = client.ChatCompletionStream(ctx, req, processor)
    }
}
```

## Session Management

For persistent conversations:

```go
import "github.com/pkdindustries/pollytool/sessions"

// Create file-based session store
store, err := sessions.NewFileSessionStore("~/.pollytool/contexts")

// Get or create session
session := store.Get("my-session-id")

// Add messages
session.AddMessage(messages.ChatMessage{
    Role:    messages.MessageRoleUser,
    Content: "Hello!",
})

// Get history for requests
history := session.GetHistory()

// Clear session
session.Clear()

// Close session (releases locks)
if fileSession, ok := session.(*sessions.FileSession); ok {
    defer fileSession.Close()
}
```

## Structured Output

Use JSON Schema for structured responses:

```go
schema := &llm.Schema{
    Name: "UserInfo",
    Schema: &jsonschema.Schema{
        Type: "object",
        Properties: map[string]*jsonschema.Schema{
            "name": {
                Type:        "string",
                Description: "User's full name",
            },
            "age": {
                Type:        "integer",
                Description: "User's age",
            },
            "email": {
                Type:        "string",
                Format:      "email",
                Description: "User's email address",
            },
        },
        Required: []string{"name", "email"},
    },
}

req := &llm.CompletionRequest{
    Model: "openai/gpt-4.1",
    Messages: []messages.ChatMessage{
        {
            Role:    messages.MessageRoleUser,
            Content: "Extract user info from: John Doe, 30 years old, john@example.com",
        },
    },
    ResponseSchema: schema,
}
```

## Error Handling

```go
eventChan := client.ChatCompletionStream(ctx, req, processor)

for event := range eventChan {
    switch event.Type {
    case messages.EventTypeError:
        // Handle errors
        if strings.Contains(event.Error.Error(), "rate limit") {
            // Implement backoff
            time.Sleep(time.Second * 5)
        } else if strings.Contains(event.Error.Error(), "context length") {
            // Truncate conversation history
            req.Messages = req.Messages[len(req.Messages)-5:]
        }
    }
}
```

## Complete Examples

### Simple Example Using Helpers

```go
package main

import (
    "context"
    "fmt"
    
    "github.com/pkdindustries/pollytool/llm"
)

func main() {
    ctx := context.Background()
    
    // Set environment variable: export POLLYTOOL_OPENAIKEY="your-key"
    
    // Simple completion
    joke, err := llm.QuickComplete(ctx, "openai/gpt-4.1", "Tell me a joke", 500)
    if err != nil {
        panic(err)
    }
    fmt.Println(joke)
    
    // Streaming completion
    fmt.Println("\nWriting a story:")
    err = llm.StreamComplete(ctx, "openai/gpt-4.1", "Write a short story", 500, func(chunk string) {
        fmt.Print(chunk)
    })
    if err != nil {
        panic(err)
    }
}
```

### Advanced Example with Sessions

```go
package main

import (
    "context"
    "fmt"
    "os"
    "time"
    
    "github.com/pkdindustries/pollytool/llm"
    "github.com/pkdindustries/pollytool/messages"
    "github.com/pkdindustries/pollytool/sessions"
)

func main() {
    ctx := context.Background()
    
    // For advanced use cases, create client manually
    apiKeys := map[string]string{
        "openai": os.Getenv("POLLYTOOL_OPENAIKEY"),
    }
    client := llm.NewMultiPass(apiKeys)
    
    // Create session
    store, _ := sessions.NewFileSessionStore("")
    session := store.Get("example-session")
    defer func() {
        if fs, ok := session.(*sessions.FileSession); ok {
            fs.Close()
        }
    }()
    
    // Add system prompt if new session
    if len(session.GetHistory()) == 0 {
        session.AddMessage(messages.ChatMessage{
            Role:    messages.MessageRoleSystem,
            Content: "You are a helpful AI assistant.",
        })
    }
    
    // Add user message
    session.AddMessage(messages.ChatMessage{
        Role:    messages.MessageRoleUser,
        Content: "Tell me a joke",
    })
    
    // Create request
    req := &llm.CompletionRequest{
        Model:       "openai/gpt-4.1",
        Messages:    session.GetHistory(),
        Temperature: 0.7,
        MaxTokens:   500,
        Timeout:     30 * time.Second,
    }
    
    // Stream response
    processor := messages.NewStreamProcessor()
    eventChan := client.ChatCompletionStream(ctx, req, processor)
    
    var response messages.ChatMessage
    fmt.Print("Assistant: ")
    
    for event := range eventChan {
        switch event.Type {
        case messages.EventTypeContent:
            fmt.Print(event.Content)
        case messages.EventTypeComplete:
            response = *event.Message
            session.AddMessage(response)
            fmt.Println()
        case messages.EventTypeError:
            fmt.Printf("\nError: %v\n", event.Error)
            return
        }
    }
}
```

## Model Identifiers

Use the format `provider/model`:

- OpenAI: `openai/gpt-4`, `openai/gpt-3.5-turbo`
- Anthropic: `anthropic/claude-opus-4-1-20250805`, `anthropic/claude-sonnet-4-20250514`
- Gemini: `gemini/gemini-2.0-flash`, `gemini/gemini-2.0-pro`
- Ollama: `ollama/llama2`, `ollama/mistral`

## Environment Variables

Set these for API authentication:

- `POLLYTOOL_OPENAIKEY` - OpenAI API key
- `POLLYTOOL_ANTHROPICKEY` - Anthropic API key
- `POLLYTOOL_GEMINIKEY` - Google Gemini API key
- `POLLYTOOL_OLLAMAKEY` - Ollama API key (optional for local)

## Thread Safety

- The `MultiPass` provider is thread-safe
- Session stores use mutexes for concurrent access
- Individual sessions should not be accessed concurrently
- Tool execution should be thread-safe in your implementation