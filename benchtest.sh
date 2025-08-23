#!/bin/bash

# Simple Polly System Test Script
set -e 

go build ./cmd/polly
POLLY_CMD="./polly"
TEST_DIR="/tmp/polly-test-$$"
mkdir -p "$TEST_DIR"

# Create test files
echo "This is a test text file." > "$TEST_DIR/test.txt"
cat > "$TEST_DIR/test_schema.json" << 'EOF'
{
    "type": "object",
    "properties": {
        "name": {"type": "string"},
        "age": {"type": "integer"}
    },
    "required": ["name"]
}
EOF

cat > "$TEST_DIR/test_tool.sh" << 'EOF'
#!/bin/bash
if [ "$1" = "--schema" ]; then
    echo '{"title": "test_tool", "description": "A test tool", "type": "object", "properties": {"message": {"type": "string"}}, "required": ["message"]}'
elif [ "$1" = "--execute" ]; then
    echo "Test tool executed"
fi
EOF
chmod +x "$TEST_DIR/test_tool.sh"

echo "=== Basic Tests ==="
echo "y" | $POLLY_CMD --create test-ctx --quiet
$POLLY_CMD --list
$POLLY_CMD --show test-ctx
echo "y" | $POLLY_CMD --delete test-ctx --quiet

echo "=== Validation Tests ==="
echo 'test' | $POLLY_CMD -m 'invalid/model' --quiet > /dev/null 2>&1 && echo "FAIL: Should reject invalid provider" || echo "PASS: Rejected invalid provider"
echo 'test' | $POLLY_CMD -m 'badformat' --quiet > /dev/null 2>&1 && echo "FAIL: Should reject bad format" || echo "PASS: Rejected bad format"
echo 'test' | $POLLY_CMD --temp 5.0 --quiet > /dev/null 2>&1 && echo "FAIL: Should reject bad temp" || echo "PASS: Rejected bad temp"
$POLLY_CMD -t '/nonexistent/tool.sh' -p 'test' > /dev/null 2>&1 && echo "FAIL: Should reject missing tool" || echo "PASS: Rejected missing tool"

echo "=== Context Settings Persistence Tests ==="
# Create test context with specific settings
echo "y" | $POLLY_CMD --create settings-test --model anthropic/claude-sonnet-4-20250514 --temp 0.5 --maxtokens 1000 --system "You are a test assistant" --quiet

# Check if settings were saved correctly
echo "Testing initial settings save..."
$POLLY_CMD --show settings-test | grep -q "Model: anthropic/claude-sonnet-4-20250514" && echo "PASS: Model setting saved" || echo "FAIL: Model setting not saved"
$POLLY_CMD --show settings-test | grep -q "Temperature: 0.50" && echo "PASS: Temperature setting saved" || echo "FAIL: Temperature setting not saved"
$POLLY_CMD --show settings-test | grep -q "Max Tokens: 1000" && echo "PASS: MaxTokens setting saved" || echo "FAIL: MaxTokens setting not saved"
$POLLY_CMD --show settings-test | grep -q "System Prompt: You are a test assistant" && echo "PASS: System prompt saved" || echo "FAIL: System prompt not saved"

# Test changing settings and persistence
echo "Testing settings changes..."
echo 'test message' | $POLLY_CMD -c settings-test --model openai/gpt-4o-mini --temp 1.5 --maxtokens 2000 --quiet

# Verify changes were persisted
echo "Testing settings persistence after change..."
$POLLY_CMD --show settings-test | grep -q "Model: openai/gpt-4o-mini" && echo "PASS: Model change persisted" || echo "FAIL: Model change not persisted"
$POLLY_CMD --show settings-test | grep -q "Temperature: 1.50" && echo "PASS: Temperature change persisted" || echo "FAIL: Temperature change not persisted"
$POLLY_CMD --show settings-test | grep -q "Max Tokens: 2000" && echo "PASS: MaxTokens change persisted" || echo "FAIL: MaxTokens change not persisted"

# Test tool settings persistence
echo "Testing tool settings persistence..."
echo 'test message' | $POLLY_CMD -c settings-test -t "$TEST_DIR/test_tool.sh" --quiet
$POLLY_CMD --show settings-test | grep -q "test_tool.sh" && echo "PASS: Tool setting persisted" || echo "FAIL: Tool setting not persisted"

# Test system prompt change triggers reset
echo "Testing system prompt change triggers reset..."
INITIAL_HISTORY_SIZE=$(echo 'show history' | $POLLY_CMD -c settings-test --quiet | wc -l)
echo 'another message' | $POLLY_CMD -c settings-test --system "New system prompt" --quiet
NEW_HISTORY_SIZE=$(echo 'show history' | $POLLY_CMD -c settings-test --quiet | wc -l)
if [ "$NEW_HISTORY_SIZE" -le "$INITIAL_HISTORY_SIZE" ]; then
    echo "PASS: System prompt change triggered conversation reset"
else
    echo "FAIL: System prompt change did not trigger reset"
fi

# Test settings inheritance without explicit flags
echo "Testing settings inheritance..."
echo 'inherited test' | $POLLY_CMD -c settings-test --quiet
$POLLY_CMD --show settings-test | grep -q "System Prompt: New system prompt" && echo "PASS: Settings inherited correctly" || echo "FAIL: Settings not inherited"

# Cleanup test context
echo "y" | $POLLY_CMD --delete settings-test --quiet

if [ -n "$POLLYTOOL_ANTHROPICKEY" ]; then
    echo "=== Anthropic Tests ==="
    echo 'Say hello' | timeout 30s $POLLY_CMD -m 'anthropic/claude-sonnet-4-20250514' --quiet
    timeout 30s $POLLY_CMD -f "$TEST_DIR/test.txt" -p 'What does this contain?' --quiet
    echo 'Extract: John is 25' | timeout 30s $POLLY_CMD --schema "$TEST_DIR/test_schema.json" --quiet
    timeout 30s $POLLY_CMD -t "$TEST_DIR/test_tool.sh" -p 'Use the test tool' --quiet
    echo 'What is 2+2?' | timeout 30s $POLLY_CMD --think --maxtokens 8192 --quiet
    timeout 30s $POLLY_CMD --mcp "npx -y @modelcontextprotocol/server-filesystem $TEST_DIR" -p 'List files in the directory' --quiet
fi

if [ -n "$POLLYTOOL_OPENAIKEY" ]; then
    echo "=== OpenAI Tests ==="
    echo 'Say hello' | timeout 30s $POLLY_CMD -m 'openai/gpt-4o-mini' --quiet
    timeout 30s $POLLY_CMD -f "$TEST_DIR/test.txt" -p 'What does this contain?' --quiet
    echo 'Extract: John is 25' | timeout 30s $POLLY_CMD --schema "$TEST_DIR/test_schema.json" --quiet
    timeout 30s $POLLY_CMD -t "$TEST_DIR/test_tool.sh" -p 'Use the test tool' --quiet
    echo 'What is 2+2?' | timeout 30s $POLLY_CMD --think --maxtokens 8192 --quiet
    timeout 30s $POLLY_CMD --mcp "npx -y @modelcontextprotocol/server-filesystem $TEST_DIR" -p 'List files in the directory' --quiet
fi

if [ -n "$POLLYTOOL_GEMINIKEY" ]; then
    echo "=== Gemini Tests ==="
    echo 'Say hello' | timeout 30s $POLLY_CMD -m 'gemini/gemini-2.5-flash' --quiet
    timeout 30s $POLLY_CMD -f "$TEST_DIR/test.txt" -p 'What does this contain?' --quiet
    echo 'Extract: John is 25' | timeout 30s $POLLY_CMD --schema "$TEST_DIR/test_schema.json" --quiet
    timeout 30s $POLLY_CMD -t "$TEST_DIR/test_tool.sh" -p 'Use the test tool' --quiet
    echo 'What is 2+2?' | timeout 30s $POLLY_CMD --think --maxtokens 8192 --quiet
    timeout 30s $POLLY_CMD --mcp "npx -y @modelcontextprotocol/server-filesystem $TEST_DIR" -p 'List files in the directory' --quiet
fi

# Cleanup
rm -rf "$TEST_DIR"
echo "=== Tests Complete ==="