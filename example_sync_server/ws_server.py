import asyncio
import json
from websockets import serve
from time import time

connected_clients = set()
last_broadcast = {}
SYNC_INTERVAL = 5  # Minimum time between syncs in seconds
LEEWAY = 2  # Time difference threshold in seconds
SEEK_THRESHOLD = 0.5  # Time difference threshold for seek events


async def broadcast_to_others(event, data, sender):
    message = json.dumps({"type": event, **data})
    for client in connected_clients:
        if client != sender:
            await client.send(message)


async def handle_client(websocket):
    connected_clients.add(websocket)
    try:
        async for message in websocket:
            data = json.loads(message)
            event_type = data.get("type")

            if event_type == "sync":
                current_time = data.get("currentTime", 0)
                is_playing = data.get("isPlaying", False)

                current_timestamp = time()
                if (
                    current_timestamp - last_broadcast.get(websocket, 0)
                    >= SYNC_INTERVAL
                ):
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
                        await broadcast_to_others("sync", sync_data, websocket)
                        last_broadcast[websocket] = current_timestamp
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
                    print(f"Broadcasting seek event: {data}")
                    await broadcast_to_others("seeked", data, websocket)
                    last_broadcast["last_sync"] = {
                        "currentTime": current_time,
                        "isPlaying": last_sync.get("isPlaying", False),
                    }
                else:
                    print(f"Seek event ignored (within threshold): {data}")

            elif event_type in ["play", "pause", "stop"]:
                print(f"{event_type.capitalize()} event received: {data}")
                await broadcast_to_others(event_type, data, websocket)
            else:
                print(f"Unknown event type: {event_type}")
    finally:
        connected_clients.remove(websocket)
        if websocket in last_broadcast:
            del last_broadcast[websocket]
        print("Client disconnected")


async def main():
    server = await serve(handle_client, "localhost", 3000)
    print("WebSocket server started on ws://localhost:3000")
    await server.wait_closed()


if __name__ == "__main__":
    asyncio.run(main())
