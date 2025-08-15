#!/usr/bin/env python3
import json
import sys

def get_weather(location):
    """Get the current weather for a location."""
    # This is a mock implementation
    return f"The weather in {location} is sunny and 72°F"

if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--schema":
        schema = {
            "title": "get_weather",
            "description": "Get the current weather for a location",
            "type": "object",
            "properties": {
                "location": {
                    "type": "string",
                    "description": "The location to get weather for"
                }
            },
            "required": ["location"]
        }
        print(json.dumps(schema))
    elif len(sys.argv) > 2 and sys.argv[1] == "--execute":
        args = json.loads(sys.argv[2])
        result = get_weather(args.get("location", "unknown"))
        print(result)
    else:
        print("Usage: weather.py --schema | weather.py --execute '{\"location\": \"New York\"}'")