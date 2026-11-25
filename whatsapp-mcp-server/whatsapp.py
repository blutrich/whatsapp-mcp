"""Data access for the WhatsApp MCP server.

Reads go straight to the bridge's SQLite database, opened read-only.
Sends and media downloads go through the bridge's local REST API, which
requires the token the bridge keeps in ``store/api_token``.

Nothing in this module writes to stdout: with the stdio transport, stdout
carries the MCP protocol. Diagnostics go to stderr through ``logging``.
"""

import json
import logging
import os
import os.path
import sqlite3
import sys
import urllib.error
import urllib.request
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Dict, Iterator, List, Optional, Tuple

import audio

logging.basicConfig(stream=sys.stderr, level=logging.INFO, format="whatsapp-mcp %(levelname)s %(message)s")
log = logging.getLogger("whatsapp-mcp")

_HERE = os.path.dirname(os.path.abspath(__file__))

# Where the bridge keeps its data. Override with WHATSAPP_STORE_DIR when the
# bridge runs from a different folder than this checkout.
STORE_DIR = os.path.abspath(
    os.environ.get("WHATSAPP_STORE_DIR") or os.path.join(_HERE, "..", "whatsapp-bridge", "store")
)
MESSAGES_DB_PATH = os.path.join(STORE_DIR, "messages.db")
SESSION_DB_PATH = os.path.join(STORE_DIR, "whatsapp.db")
API_TOKEN_PATH = os.path.join(STORE_DIR, "api_token")
OUTBOX_DIR = os.path.join(STORE_DIR, "outbox")

MAX_LIMIT = 200
MAX_CONTEXT = 50
API_TIMEOUT_SECONDS = 30
MEDIA_TIMEOUT_SECONDS = 330


class WhatsAppError(Exception):
    """A problem worth showing to the caller as a tool error."""


def _api_base_url() -> str:
    raw = (os.environ.get("WHATSAPP_BRIDGE_PORT") or "8080").strip()
    if not raw.isdigit() or not 1 <= int(raw) <= 65535:
        raise WhatsAppError(f"WHATSAPP_BRIDGE_PORT must be a port number, got {raw!r}")
    # Always the loopback address: the bridge never listens anywhere else.
    return f"http://127.0.0.1:{int(raw)}/api"


@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    chat_name: Optional[str] = None
    media_type: Optional[str] = None


@dataclass
class Chat:
    jid: str
    name: Optional[str]
    last_message_time: Optional[datetime]
    last_message: Optional[str] = None
    last_sender: Optional[str] = None
    last_is_from_me: Optional[bool] = None

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")


@dataclass
class Contact:
    phone_number: Optional[str]
    name: Optional[str]
    jid: str


@dataclass
class MessageContext:
    message: Message
    before: List[Message]
    after: List[Message]


# --------------------------------------------------------------------------
# Database access
# --------------------------------------------------------------------------

@contextmanager
def _connect(path: str = MESSAGES_DB_PATH) -> Iterator[sqlite3.Connection]:
    """Open a bridge database read-only. Never creates or modifies it."""
    if not os.path.isfile(path):
        raise WhatsAppError(
            "The WhatsApp bridge has no message database yet. Start the bridge "
            "(whatsapp-bridge) and pair it with your phone first."
        )
    try:
        conn = sqlite3.connect(f"file:{urllib.request.pathname2url(path)}?mode=ro", uri=True, timeout=5)
    except sqlite3.Error as exc:
        raise WhatsAppError(f"Could not open the WhatsApp database: {exc}") from exc
    try:
        yield conn
    except sqlite3.Error as exc:
        raise WhatsAppError(f"WhatsApp database error: {exc}") from exc
    finally:
        conn.close()


def _parse_timestamp(value: Any) -> Optional[datetime]:
    """Parse a timestamp as stored by the bridge (ISO text with a UTC offset)."""
    if value is None or value == "":
        return None
    if isinstance(value, datetime):
        return value
    text = str(value).strip()
    try:
        return datetime.fromisoformat(text)
    except ValueError:
        pass
    # Tolerate sub-microsecond precision and a trailing zone name.
    head = text.split(" m=")[0]
    if "." in head:
        whole, _, rest = head.partition(".")
        digits = ""
        for ch in rest:
            if ch.isdigit():
                digits += ch
            else:
                break
        tail = rest[len(digits):].split(" ")[0]
        head = f"{whole}.{digits[:6]}{tail}"
    try:
        return datetime.fromisoformat(head.replace(" UTC", "+00:00"))
    except ValueError:
        log.warning("Unreadable timestamp in database")
        return None


def _to_utc_text(value: str, field: str) -> str:
    """Turn an ISO-8601 filter into the UTC text SQLite's datetime() produces.

    A value without an offset is taken as local time.
    """
    try:
        parsed = datetime.fromisoformat(value.strip().replace("Z", "+00:00"))
    except (ValueError, AttributeError):
        raise WhatsAppError(
            f"Invalid date for '{field}': {value!r}. Use ISO-8601, for example 2026-01-31 or 2026-01-31T18:00:00+02:00."
        )
    if parsed.tzinfo is None:
        parsed = parsed.astimezone()
    return parsed.astimezone(timezone.utc).strftime("%Y-%m-%d %H:%M:%S")


def _like_escape(text: str) -> str:
    """Escape LIKE wildcards so user input is matched literally (ESCAPE '\\')."""
    return text.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")


def _user_part(jid_or_number: str) -> str:
    """'9725...@s.whatsapp.net', '+972 50-...' or '9725...:3@lid' -> digits/user part."""
    text = (jid_or_number or "").strip()
    user = text.split("@", 1)[0].split(":", 1)[0]
    if "@" not in text:
        user = user.lstrip("+").replace(" ", "").replace("-", "").replace("(", "").replace(")", "")
    return user


def _clamp(value: Any, low: int, high: int, default: int) -> int:
    try:
        number = int(value)
    except (TypeError, ValueError):
        return default
    return max(low, min(high, number))


def _sender_clause(column: str) -> str:
    """SQL matching a sender stored either as a bare user or as a full JID."""
    return f"({column} = ? OR {column} LIKE ? ESCAPE '\\')"


def _sender_params(sender: str) -> List[str]:
    user = _user_part(sender)
    return [user, _like_escape(user) + "@%"]


_MESSAGE_COLUMNS = (
    "messages.timestamp, messages.sender, chats.name, messages.content, "
    "messages.is_from_me, messages.chat_jid, messages.id, messages.media_type, messages.rowid"
)


def _row_to_message(row: Tuple) -> Message:
    return Message(
        timestamp=_parse_timestamp(row[0]) or datetime.fromtimestamp(0, timezone.utc),
        sender=row[1] or "",
        chat_name=row[2],
        content=row[3] or "",
        is_from_me=bool(row[4]),
        chat_jid=row[5],
        id=row[6],
        media_type=row[7] or None,
    )


# --------------------------------------------------------------------------
# Names
# --------------------------------------------------------------------------

_name_cache: Dict[str, str] = {}


def _session_lookup(user: str) -> Optional[str]:
    """Look a sender up in the bridge's contact list (read-only, best effort).

    Group members increasingly appear under a WhatsApp "LID" instead of a
    phone number; the session database maps those back to saved contacts.
    """
    if not os.path.isfile(SESSION_DB_PATH):
        return None
    try:
        conn = sqlite3.connect(f"file:{urllib.request.pathname2url(SESSION_DB_PATH)}?mode=ro", uri=True, timeout=2)
    except sqlite3.Error:
        return None
    try:
        cursor = conn.cursor()
        candidates = [f"{user}@s.whatsapp.net", f"{user}@lid"]
        try:
            cursor.execute("SELECT pn FROM whatsmeow_lid_map WHERE lid = ? LIMIT 1", (user,))
            mapped = cursor.fetchone()
            if mapped and mapped[0]:
                candidates.append(f"{_user_part(str(mapped[0]))}@s.whatsapp.net")
        except sqlite3.Error:
            pass
        for jid in candidates:
            cursor.execute(
                "SELECT full_name, first_name, business_name, push_name FROM whatsmeow_contacts WHERE their_jid = ? LIMIT 1",
                (jid,),
            )
            row = cursor.fetchone()
            if row:
                for value in row:
                    if value:
                        return str(value)
        return None
    except sqlite3.Error:
        return None
    finally:
        conn.close()


def get_sender_name(sender_jid: str) -> str:
    """Best display name for a sender, falling back to the raw identifier."""
    if not sender_jid:
        return "Unknown"
    if sender_jid in _name_cache:
        return _name_cache[sender_jid]
    user = _user_part(sender_jid)
    name: Optional[str] = None
    try:
        with _connect() as conn:
            cursor = conn.cursor()
            # Exact matches only: a partial match could attribute a message
            # to the wrong person.
            cursor.execute(
                "SELECT name FROM chats WHERE jid IN (?, ?, ?) AND name IS NOT NULL AND name != '' LIMIT 1",
                (sender_jid, f"{user}@s.whatsapp.net", f"{user}@lid"),
            )
            row = cursor.fetchone()
            if row and row[0] and row[0] != user:
                name = row[0]
    except WhatsAppError:
        name = None
    if not name:
        name = _session_lookup(user)
    result = name or sender_jid
    if len(_name_cache) < 5000:
        _name_cache[sender_jid] = result
    return result


# --------------------------------------------------------------------------
# Formatting
# --------------------------------------------------------------------------

def _clean_text(text: Optional[str], single_line: bool = False) -> str:
    """Remove control characters from text written by other people.

    Message text, chat names and sender names are untrusted. Stripping control
    characters and indenting continuation lines keeps a message from imitating
    the header line of another message in the formatted output.
    """
    if not text:
        return ""
    cleaned = []
    for ch in str(text):
        code = ord(ch)
        if ch == "\n" or ch == "\t":
            cleaned.append(" " if single_line else ch)
        elif code < 32 or 127 <= code < 160 or code in (0x2028, 0x2029):
            cleaned.append(" ")
        else:
            cleaned.append(ch)
    result = "".join(cleaned)
    if single_line:
        return " ".join(result.split())
    return result.replace("\n", "\n    ")


def format_message(message: Message, show_chat_info: bool = True) -> str:
    """Render a single message on one header line, continuation lines indented."""
    output = f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "
    if show_chat_info and message.chat_name:
        output += f"Chat: {_clean_text(message.chat_name, single_line=True)} "

    content_prefix = ""
    if message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "

    sender_name = "Me" if message.is_from_me else _clean_text(get_sender_name(message.sender), single_line=True)
    output += f"From: {sender_name}: {content_prefix}{_clean_text(message.content)}\n"
    return output


def format_messages_list(messages: List[Message], show_chat_info: bool = True) -> str:
    if not messages:
        return "No messages to display."
    return "".join(format_message(message, show_chat_info) for message in messages)


# --------------------------------------------------------------------------
# Queries
# --------------------------------------------------------------------------

def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1,
) -> str:
    """Get messages matching the specified criteria with optional context."""
    limit = _clamp(limit, 1, MAX_LIMIT, 20)
    page = _clamp(page, 0, 1_000_000, 0)
    context_before = _clamp(context_before, 0, MAX_CONTEXT, 1)
    context_after = _clamp(context_after, 0, MAX_CONTEXT, 1)

    where_clauses: List[str] = []
    params: List[Any] = []

    # Stored timestamps carry a UTC offset; datetime() normalises both sides
    # to UTC so filters and ordering stay correct across daylight-saving
    # changes and time-zone moves.
    if after:
        where_clauses.append("datetime(messages.timestamp) > ?")
        params.append(_to_utc_text(after, "after"))
    if before:
        where_clauses.append("datetime(messages.timestamp) < ?")
        params.append(_to_utc_text(before, "before"))
    if sender_phone_number:
        where_clauses.append(_sender_clause("messages.sender"))
        params.extend(_sender_params(sender_phone_number))
    if chat_jid:
        where_clauses.append("messages.chat_jid = ?")
        params.append(chat_jid)
    if query:
        where_clauses.append("messages.content LIKE ? ESCAPE '\\'")
        params.append(f"%{_like_escape(query)}%")

    sql = f"SELECT {_MESSAGE_COLUMNS} FROM messages JOIN chats ON messages.chat_jid = chats.jid"
    if where_clauses:
        sql += " WHERE " + " AND ".join(where_clauses)
    sql += " ORDER BY datetime(messages.timestamp) DESC, messages.rowid DESC LIMIT ? OFFSET ?"
    params.extend([limit, page * limit])

    with _connect() as conn:
        rows = conn.execute(sql, tuple(params)).fetchall()
        matches = [(_row_to_message(row), row[8]) for row in rows]

        if not include_context or not matches:
            return format_messages_list([m for m, _ in matches], show_chat_info=True)

        # Add context, without repeating a message that several matches share.
        seen = set()
        ordered: List[Message] = []
        for message, rowid in matches:
            before_rows, after_rows = _context_rows(conn, message.chat_jid, _utc_of(conn, rowid), rowid, context_before, context_after)
            for row in before_rows + [None] + after_rows:
                item, item_rowid = (message, rowid) if row is None else (_row_to_message(row), row[8])
                if item_rowid not in seen:
                    seen.add(item_rowid)
                    ordered.append(item)
        return format_messages_list(ordered, show_chat_info=True)


def _utc_of(conn: sqlite3.Connection, rowid: int) -> str:
    """UTC text of a message's timestamp, as SQLite sees it."""
    row = conn.execute("SELECT datetime(timestamp) FROM messages WHERE rowid = ?", (rowid,)).fetchone()
    return row[0] if row and row[0] else ""


def _context_rows(conn: sqlite3.Connection, chat_jid: str, utc_text: str, rowid: int, before: int, after: int):
    """Messages just before (oldest first) and just after a message in its chat."""
    before_rows: List[Tuple] = []
    after_rows: List[Tuple] = []
    if before > 0:
        before_rows = conn.execute(
            f"""
            SELECT {_MESSAGE_COLUMNS} FROM messages JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND (datetime(messages.timestamp), messages.rowid) < (?, ?)
            ORDER BY datetime(messages.timestamp) DESC, messages.rowid DESC LIMIT ?
            """,
            (chat_jid, utc_text, rowid, before),
        ).fetchall()
        before_rows.reverse()
    if after > 0:
        after_rows = conn.execute(
            f"""
            SELECT {_MESSAGE_COLUMNS} FROM messages JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND (datetime(messages.timestamp), messages.rowid) > (?, ?)
            ORDER BY datetime(messages.timestamp) ASC, messages.rowid ASC LIMIT ?
            """,
            (chat_jid, utc_text, rowid, after),
        ).fetchall()
    return before_rows, after_rows


def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5,
    chat_jid: Optional[str] = None,
) -> MessageContext:
    """Get context around a specific message.

    Message IDs are only unique within a chat, so pass chat_jid when known.
    """
    before = _clamp(before, 0, MAX_CONTEXT, 5)
    after = _clamp(after, 0, MAX_CONTEXT, 5)
    with _connect() as conn:
        sql = f"SELECT {_MESSAGE_COLUMNS} FROM messages JOIN chats ON messages.chat_jid = chats.jid WHERE messages.id = ?"
        params: List[Any] = [message_id]
        if chat_jid:
            sql += " AND messages.chat_jid = ?"
            params.append(chat_jid)
        sql += " ORDER BY datetime(messages.timestamp) DESC LIMIT 2"
        rows = conn.execute(sql, tuple(params)).fetchall()
        if not rows:
            raise WhatsAppError(f"Message with ID {message_id} not found")
        if len(rows) > 1:
            raise WhatsAppError(
                f"Message ID {message_id} exists in more than one chat. Pass chat_jid to say which one."
            )
        row = rows[0]
        target = _row_to_message(row)
        before_rows, after_rows = _context_rows(conn, target.chat_jid, _utc_of(conn, row[8]), row[8], before, after)
        return MessageContext(
            message=target,
            before=[_row_to_message(r) for r in before_rows],
            after=[_row_to_message(r) for r in after_rows],
        )


def _last_message(conn: sqlite3.Connection, chat_jid: str) -> Tuple[Optional[str], Optional[str], Optional[bool]]:
    row = conn.execute(
        """
        SELECT content, sender, is_from_me, media_type FROM messages
        WHERE chat_jid = ? ORDER BY datetime(timestamp) DESC, rowid DESC LIMIT 1
        """,
        (chat_jid,),
    ).fetchone()
    if not row:
        return None, None, None
    content = row[0] or (f"[{row[3]}]" if row[3] else "")
    return content, row[1], bool(row[2])


def _rows_to_chats(conn: sqlite3.Connection, rows: List[Tuple], include_last_message: bool = True) -> List[Chat]:
    chats = []
    for jid, name, last_time in rows:
        chat = Chat(jid=jid, name=name, last_message_time=_parse_timestamp(last_time))
        if include_last_message:
            chat.last_message, chat.last_sender, chat.last_is_from_me = _last_message(conn, jid)
        chats.append(chat)
    return chats


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active",
) -> List[Chat]:
    """Get chats matching the specified criteria. One row per chat."""
    limit = _clamp(limit, 1, MAX_LIMIT, 20)
    page = _clamp(page, 0, 1_000_000, 0)
    sql = "SELECT jid, name, last_message_time FROM chats"
    params: List[Any] = []
    if query:
        pattern = f"%{_like_escape(query)}%"
        sql += " WHERE (name LIKE ? ESCAPE '\\' OR jid LIKE ? ESCAPE '\\')"
        params.extend([pattern, pattern])
    # sort_by is matched against known values, never placed into the SQL.
    sql += " ORDER BY name COLLATE NOCASE, jid" if sort_by == "name" else " ORDER BY datetime(last_message_time) DESC, jid"
    sql += " LIMIT ? OFFSET ?"
    params.extend([limit, page * limit])
    with _connect() as conn:
        rows = conn.execute(sql, tuple(params)).fetchall()
        return _rows_to_chats(conn, rows, include_last_message)


def _lid_phone(user: str) -> Optional[str]:
    """Phone number behind a LID, when the bridge has learned it."""
    if not os.path.isfile(SESSION_DB_PATH):
        return None
    try:
        conn = sqlite3.connect(f"file:{urllib.request.pathname2url(SESSION_DB_PATH)}?mode=ro", uri=True, timeout=2)
    except sqlite3.Error:
        return None
    try:
        row = conn.execute("SELECT pn FROM whatsmeow_lid_map WHERE lid = ? LIMIT 1", (user,)).fetchone()
        return _user_part(str(row[0])) if row and row[0] else None
    except sqlite3.Error:
        return None
    finally:
        conn.close()


def search_contacts(query: str) -> List[Contact]:
    """Search contacts by name or phone number."""
    pattern = f"%{_like_escape(query or '')}%"
    with _connect() as conn:
        rows = conn.execute(
            """
            SELECT DISTINCT jid, name FROM chats
            WHERE (name LIKE ? ESCAPE '\\' OR jid LIKE ? ESCAPE '\\')
              AND (jid LIKE '%@s.whatsapp.net' OR jid LIKE '%@lid')
            ORDER BY name COLLATE NOCASE, jid
            LIMIT 50
            """,
            (pattern, pattern),
        ).fetchall()
    contacts = []
    for jid, name in rows:
        user = _user_part(jid)
        # A "@lid" address is WhatsApp's privacy identifier, not a phone number.
        phone = _lid_phone(user) if jid.endswith("@lid") else user
        contacts.append(Contact(phone_number=phone, name=name, jid=jid))
    return contacts


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    """Get all chats involving the contact: their direct chat and shared groups."""
    limit = _clamp(limit, 1, MAX_LIMIT, 20)
    page = _clamp(page, 0, 1_000_000, 0)
    user = _user_part(jid)
    if not user:
        return []
    with _connect() as conn:
        rows = conn.execute(
            f"""
            SELECT c.jid, c.name, c.last_message_time FROM chats c
            WHERE c.jid IN (?, ?, ?)
               OR c.jid IN (SELECT DISTINCT m.chat_jid FROM messages m WHERE {_sender_clause('m.sender')})
            ORDER BY datetime(c.last_message_time) DESC, c.jid
            LIMIT ? OFFSET ?
            """,
            (jid, f"{user}@s.whatsapp.net", f"{user}@lid", *_sender_params(jid), limit, page * limit),
        ).fetchall()
        return _rows_to_chats(conn, rows)


def get_last_interaction(jid: str) -> Optional[str]:
    """Get most recent message involving the contact."""
    user = _user_part(jid)
    if not user:
        return None
    with _connect() as conn:
        row = conn.execute(
            f"""
            SELECT {_MESSAGE_COLUMNS} FROM messages JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid IN (?, ?, ?) OR {_sender_clause('messages.sender')}
            ORDER BY datetime(messages.timestamp) DESC, messages.rowid DESC
            LIMIT 1
            """,
            (jid, f"{user}@s.whatsapp.net", f"{user}@lid", *_sender_params(jid)),
        ).fetchone()
    if not row:
        return None
    return format_message(_row_to_message(row))


def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Chat]:
    """Get chat metadata by JID."""
    with _connect() as conn:
        rows = conn.execute("SELECT jid, name, last_message_time FROM chats WHERE jid = ?", (chat_jid,)).fetchall()
        chats = _rows_to_chats(conn, rows, include_last_message)
    return chats[0] if chats else None


def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Chat]:
    """Get the one-to-one chat with a phone number (exact match, not a partial one)."""
    user = _user_part(sender_phone_number)
    if not user:
        return None
    with _connect() as conn:
        rows = conn.execute(
            """
            SELECT jid, name, last_message_time FROM chats
            WHERE jid IN (?, ?)
            ORDER BY datetime(last_message_time) DESC LIMIT 1
            """,
            (f"{user}@s.whatsapp.net", f"{user}@lid"),
        ).fetchall()
        chats = _rows_to_chats(conn, rows)
    return chats[0] if chats else None


# --------------------------------------------------------------------------
# Bridge API (send, download)
# --------------------------------------------------------------------------

def _read_api_token() -> str:
    try:
        with open(API_TOKEN_PATH, "r", encoding="ascii") as handle:
            token = handle.read().strip()
    except OSError:
        raise WhatsAppError(
            "The WhatsApp bridge is not running or was started from a different folder "
            f"(no API token at {API_TOKEN_PATH}). Start the bridge, or set WHATSAPP_STORE_DIR."
        )
    if len(token) != 64:
        raise WhatsAppError("The bridge API token is malformed. Restart the bridge to create a new one.")
    return token


# No proxy, ever: requests to the bridge stay on this machine.
_opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def _api_post(endpoint: str, payload: Dict[str, Any], timeout: int = API_TIMEOUT_SECONDS) -> Tuple[int, Dict[str, Any]]:
    """POST JSON to the bridge. Returns (HTTP status, decoded body)."""
    request = urllib.request.Request(
        f"{_api_base_url()}/{endpoint}",
        data=json.dumps(payload).encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {_read_api_token()}",
        },
        method="POST",
    )
    try:
        with _opener.open(request, timeout=timeout) as response:
            status, body = response.status, response.read(1 << 20)
    except urllib.error.HTTPError as exc:
        status, body = exc.code, exc.read(1 << 20)
    except (urllib.error.URLError, OSError) as exc:
        raise WhatsAppError(
            "Could not reach the WhatsApp bridge on this machine. Make sure it is running "
            f"(cd whatsapp-bridge && go run .). Details: {getattr(exc, 'reason', exc)}"
        )
    text = body.decode("utf-8", errors="replace").strip()
    try:
        data = json.loads(text)
        if not isinstance(data, dict):
            data = {"message": text}
    except ValueError:
        data = {"message": text}
    if status == 401:
        data["message"] = "The bridge rejected the API token. Restart the MCP server and the bridge from the same folder."
    return status, data


def _send(payload: Dict[str, Any], timeout: int) -> Tuple[bool, str]:
    try:
        status, data = _api_post("send", payload, timeout)
    except WhatsAppError as exc:
        return False, str(exc)
    message = str(data.get("message") or f"HTTP {status}")
    return (status == 200 and bool(data.get("success"))), message


def _check_outgoing_file(media_path: str) -> Optional[str]:
    """Quick local checks; the bridge enforces the full outgoing-file policy."""
    if not media_path:
        return "Media path must be provided"
    if not os.path.isabs(media_path):
        return "Media path must be an absolute path"
    if not os.path.isfile(media_path):
        return f"Media file not found: {media_path}"
    return None


def send_message(recipient: str, message: str) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"
    if not message:
        return False, "Message text must be provided"
    return _send({"recipient": recipient, "message": message}, API_TIMEOUT_SECONDS + 60)


def send_file(recipient: str, media_path: str) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"
    problem = _check_outgoing_file(media_path)
    if problem:
        return False, problem
    return _send({"recipient": recipient, "media_path": media_path}, MEDIA_TIMEOUT_SECONDS)


def send_audio_message(recipient: str, media_path: str) -> Tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"
    problem = _check_outgoing_file(media_path)
    if problem:
        return False, problem

    converted: Optional[str] = None
    if not media_path.lower().endswith(".ogg"):
        try:
            converted = audio.convert_to_opus_ogg_temp(media_path, OUTBOX_DIR)
        except Exception as exc:
            return False, f"Error converting file to opus ogg. You likely need to install ffmpeg: {exc}"
    try:
        return _send({"recipient": recipient, "media_path": converted or media_path}, MEDIA_TIMEOUT_SECONDS)
    finally:
        if converted:
            try:
                os.unlink(converted)
            except OSError:
                pass


def download_media(message_id: str, chat_jid: str) -> Tuple[Optional[str], str]:
    """Ask the bridge to download a message's media.

    Returns (local file path or None, status message).
    """
    if not message_id or not chat_jid:
        return None, "message_id and chat_jid are required"
    try:
        status, data = _api_post("download", {"message_id": message_id, "chat_jid": chat_jid}, MEDIA_TIMEOUT_SECONDS)
    except WhatsAppError as exc:
        return None, str(exc)
    if status == 200 and data.get("success") and data.get("path"):
        return str(data["path"]), "Media downloaded successfully"
    return None, str(data.get("message") or f"HTTP {status}")
