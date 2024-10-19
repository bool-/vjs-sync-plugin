import asyncio
import socketio
from aiohttp import web
from time import time

sio = socketio.AsyncServer(async_mode="aiohttp", cors_allowed_origins="*")
app = web.Application()
sio.attach(app)

connected_clients = set()
last_broadcast = {}
SYNC_INTERVAL = 5  # Minimum time between syncs in seconds
LEEWAY = 2  # Time difference threshold in seconds
SEEK_THRESHOLD = 0.5  # Time difference threshold for seek events


async def broadcast_to_others(event, data, sender_sid):
    for client_sid in connected_clients:
        if client_sid != sender_sid:
            await sio.emit("sync_event", {"type": event, **data}, room=client_sid)


@sio.event
async def connect(sid, environ):
    connected_clients.add(sid)
    print(f"Client connected: {sid}")


@sio.event
async def disconnect(sid):
    connected_clients.remove(sid)
    if sid in last_broadcast:
        del last_broadcast[sid]
    print(f"Client disconnected: {sid}")


@sio.event
async def sync_event(sid, data):
    event_type = data.get("type")
    if event_type == "sync":
        current_time = data.get("currentTime", 0)
        is_playing = data.get("isPlaying", False)
        current_timestamp = time()
        if current_timestamp - last_broadcast.get(sid, 0) >= SYNC_INTERVAL:
            last_sync = last_broadcast.get(
                "last_sync", {"currentTime": 0, "isPlaying": False}
            )
            time_diff = abs(current_time - last_sync["currentTime"])
            state_changed = is_playing != last_sync["isPlaying"]
            if time_diff > LEEWAY or state_changed:
                sync_data = {
                    "type": "sync",
                    "currentTime": current_time,
                    "isPlaying": is_playing,
                }
                print(f"Broadcasting sync event: {sync_data}")
                await broadcast_to_others("sync", sync_data, sid)
                last_broadcast[sid] = current_timestamp
                last_broadcast["last_sync"] = sync_data
            else:
                print(f"Sync event ignored (within leeway): {data}")
        else:
            print(f"Sync event ignored (too frequent): {data}")
    elif event_type == "seeked":
        current_time = data.get("currentTime", 0)
        last_sync = last_broadcast.get("last_sync", {"currentTime": 0})
        time_diff = abs(current_time - last_sync["currentTime"])
        if time_diff > SEEK_THRESHOLD:
            print(f"Broadcasting seeked event: {data}")
            await broadcast_to_others("seeked", data, sid)
            last_broadcast["last_sync"] = {
                "currentTime": current_time,
                "isPlaying": last_sync.get("isPlaying", False),
            }
        else:
            print(f"Seek event ignored (within threshold): {data}")
    elif event_type in ["play", "pause", "stop"]:
        print(f"{event_type.capitalize()} event received: {data}")
        await broadcast_to_others(event_type, data, sid)
    else:
        print(f"Unknown event type: {event_type}")


async def main():
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "localhost", 3000)
    await site.start()
    print("Socket.IO server started on http://localhost:3000")
    await asyncio.Event().wait()  # Run forever


if __name__ == "__main__":
    asyncio.run(main())
