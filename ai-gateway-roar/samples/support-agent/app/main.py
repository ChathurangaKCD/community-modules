"""Minimal support-triage agent. Knows nothing about Roar: the
roar-ai-gateway trait injects OPENAI_BASE_URL, OPENAI_API_KEY and LLM_MODEL."""
import json
import os

from fastapi import FastAPI
from openai import OpenAI
from pydantic import BaseModel

client = OpenAI()  # reads OPENAI_BASE_URL + OPENAI_API_KEY
MODEL = os.environ.get("LLM_MODEL", "llama-3.3-70b")
app = FastAPI()

TOOLS = [{
    "type": "function",
    "function": {
        "name": "route_ticket",
        "description": "Route a support ticket to a team with a priority.",
        "parameters": {
            "type": "object",
            "properties": {
                "team": {"type": "string", "enum": ["billing", "platform", "security", "other"]},
                "priority": {"type": "string", "enum": ["p1", "p2", "p3"]},
                "summary": {"type": "string"},
            },
            "required": ["team", "priority", "summary"],
        },
    },
}]


class Ticket(BaseModel):
    text: str


@app.get("/healthz")
def healthz():
    return {"ok": True, "model": MODEL}


@app.post("/triage")
def triage(ticket: Ticket):
    resp = client.chat.completions.create(
        model=MODEL,
        messages=[
            {"role": "system", "content": "You triage support tickets. Always call route_ticket."},
            {"role": "user", "content": ticket.text},
        ],
        tools=TOOLS,
        tool_choice={"type": "function", "function": {"name": "route_ticket"}},
    )
    call = resp.choices[0].message.tool_calls[0]
    # Roar returns x-roar-request-id; handy to log for cost/debug correlation.
    return {"decision": json.loads(call.function.arguments), "model": resp.model}
