"""P2: an AgentExecutor whose first event is TaskUpdater.submit() fails
every request with "Agent should enqueue Task before TaskStatusUpdateEvent
event". In 1.x the first event of a new task must be the Task itself
(new_task(...)); submit() reads like the call that creates the task.

Server and client are both a2a-sdk, joined in process through httpx's ASGI
transport (no network).

Run: python p2_taskupdater_submit.py
Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
"""

import asyncio
import logging
import sys
import uuid

import httpx

from starlette.applications import Starlette

from a2a.client import ClientConfig, ClientFactory
from a2a.helpers.proto_helpers import new_task, new_text_message
from a2a.server.agent_execution import AgentExecutor, RequestContext
from a2a.server.events import EventQueue
from a2a.server.request_handlers import DefaultRequestHandler
from a2a.server.routes import create_jsonrpc_routes
from a2a.server.tasks import InMemoryTaskStore, TaskUpdater
from a2a.types import (AgentCapabilities, AgentCard, AgentInterface, AgentSkill, Message, Part, Role,
                       SendMessageRequest, TaskState)


class SubmitFirst(AgentExecutor):
    """Starts a new task with TaskUpdater.submit(), then completes it."""

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        up = TaskUpdater(event_queue, context.task_id, context.context_id)
        await up.submit()
        await up.complete(new_text_message('done', context_id=context.context_id, task_id=context.task_id))

    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        await TaskUpdater(event_queue, context.task_id, context.context_id).cancel()


class TaskFirst(SubmitFirst):
    """Control: enqueues the Task itself first, as 1.x requires."""

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        await event_queue.enqueue_event(new_task(context.task_id, context.context_id, TaskState.TASK_STATE_SUBMITTED))
        up = TaskUpdater(event_queue, context.task_id, context.context_id)
        await up.complete(new_text_message('done', context_id=context.context_id, task_id=context.task_id))


def agent_card() -> AgentCard:
    return AgentCard(
        name='p2', description='d', version='1', default_input_modes=['text/plain'],
        default_output_modes=['text/plain'], capabilities=AgentCapabilities(streaming=False),
        supported_interfaces=[AgentInterface(url='http://test/', protocol_binding='JSONRPC', protocol_version='1.0')],
        skills=[AgentSkill(id='s', name='s', description='d', tags=['t'])])


async def run(executor: AgentExecutor) -> str:
    card = agent_card()
    handler = DefaultRequestHandler(agent_executor=executor, task_store=InMemoryTaskStore(), agent_card=card)
    app = Starlette(routes=create_jsonrpc_routes(handler, '/'))
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url='http://test') as hx:
        client = ClientFactory(ClientConfig(streaming=False, httpx_client=hx)).create(card)
        msg = Message(message_id=str(uuid.uuid4()), role=Role.ROLE_USER, parts=[Part(text='hi')])
        try:
            async for ev in client.send_message(SendMessageRequest(message=msg)):
                task = ev.task if ev.HasField('task') else None
                if task is not None:
                    return 'task %s' % TaskState.Name(task.status.state)
                return 'message'
            return 'no response'
        except Exception as e:
            return '%s: %s' % (type(e).__name__, str(e).splitlines()[0][:100])


async def main() -> int:
    logging.disable(logging.CRITICAL)  # the server logs the failure with a traceback
    submit_first = await run(SubmitFirst())
    task_first = await run(TaskFirst())
    print('executor starting with TaskUpdater.submit(): %s' % submit_first)
    print('executor starting with new_task(...):        %s (control)' % task_first)
    print('expected: both complete, or submit() documented as not creating the task')
    reproduced = 'COMPLETED' not in submit_first and 'COMPLETED' in task_first
    print('reproduced: %s' % ('yes' if reproduced else 'no'))
    return 0 if reproduced else 1


if __name__ == '__main__':
    sys.exit(asyncio.run(main()))
