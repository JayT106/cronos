from typing import List, Optional

from .types import PeerPacket


def connect_all(peer, peers: List[PeerPacket]) -> str:
    """
    connect the peer to all the other peers

    returns the value for persistent-peers config
    """
    return ",".join(other.peer_id for other in peers if other.peer_id != peer.peer_id)


def bootstrap_peers(peer: PeerPacket, peers: List[PeerPacket]) -> Optional[List[dict]]:
    """
    generate libp2p bootstrap_peers config for the given peer,
    connecting to all other peers.

    returns None if any peer is missing a libp2p_id.
    """
    if not all(p.libp2p_id for p in peers):
        return None
    return [
        {"host": f"{other.ip}:26656", "id": other.libp2p_id}
        for other in peers
        if other.peer_id != peer.peer_id
    ]
