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
import time
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
        chat_name=_chat_display_name(row[5], row[2]),
        content=row[3] or "",
        is_from_me=bool(row[4]),
        chat_jid=row[5],
        id=row[6],
        media_type=row[7] or None,
    )


# --------------------------------------------------------------------------
# Contact directory (names, phone numbers, privacy identifiers)
# --------------------------------------------------------------------------
#
# WhatsApp addresses a person either by phone number (…@s.whatsapp.net) or by
# a privacy identifier (…@lid). The bridge's session database holds the
# address book and the mapping between the two. The directory below joins
# them so that a chat stored under an identifier still gets its saved name,
# and so that searching finds people you have never chatted with.

PN_SERVER = "s.whatsapp.net"
LID_SERVER = "lid"
_DIRECTORY_TTL_SECONDS = 30


@dataclass
class Person:
    """One person, however WhatsApp addresses them."""
    name: str
    phone_number: Optional[str] = None
    lid: Optional[str] = None
    other_names: Tuple[str, ...] = ()

    @property
    def jids(self) -> List[str]:
        found = []
        if self.phone_number:
            found.append(f"{self.phone_number}@{PN_SERVER}")
        if self.lid:
            found.append(f"{self.lid}@{LID_SERVER}")
        return found

    @property
    def jid(self) -> Optional[str]:
        """The address to send to: the phone number when known."""
        jids = self.jids
        return jids[0] if jids else None


class _Directory:
    def __init__(self) -> None:
        self.people: List[Person] = []
        self.by_user: Dict[str, Person] = {}

    def person_for(self, jid_or_user: str) -> Optional[Person]:
        return self.by_user.get(_user_part(jid_or_user))


_directory_cache: Tuple[float, Optional[_Directory]] = (0.0, None)


def _looks_like_number(name: Optional[str]) -> bool:
    """True for an empty name or one that is only a phone number or identifier."""
    stripped = (name or "").strip().lstrip("+")
    return not stripped or stripped.replace(" ", "").replace("-", "").isdigit()


def _load_directory() -> _Directory:
    directory = _Directory()
    pn_of_lid: Dict[str, str] = {}
    contact_rows: List[Tuple] = []
    if os.path.isfile(SESSION_DB_PATH):
        try:
            conn = sqlite3.connect(f"file:{urllib.request.pathname2url(SESSION_DB_PATH)}?mode=ro", uri=True, timeout=2)
            try:
                try:
                    for lid, pn in conn.execute("SELECT lid, pn FROM whatsmeow_lid_map"):
                        if lid and pn:
                            pn_of_lid[_user_part(str(lid))] = _user_part(str(pn))
                except sqlite3.Error:
                    pass
                try:
                    contact_rows = conn.execute(
                        "SELECT their_jid, full_name, first_name, business_name, push_name FROM whatsmeow_contacts"
                    ).fetchall()
                except sqlite3.Error:
                    contact_rows = []
            finally:
                conn.close()
        except sqlite3.Error:
            pass

    # Chats the bridge named itself (older data, or a contact saved later).
    chat_rows: List[Tuple] = []
    if os.path.isfile(MESSAGES_DB_PATH):
        try:
            with _connect() as conn:
                chat_rows = conn.execute(
                    "SELECT jid, name FROM chats WHERE jid LIKE '%@s.whatsapp.net' OR jid LIKE '%@lid'"
                ).fetchall()
        except WhatsAppError:
            chat_rows = []

    # identity key -> [phone, lid, names in order of preference]
    merged: Dict[str, List[Any]] = {}

    def entry_for(jid: str) -> Optional[List[Any]]:
        if not jid or "@" not in jid:
            return None
        user, server = _user_part(jid), jid.rsplit("@", 1)[1]
        if not user:
            return None
        if server == PN_SERVER:
            phone, lid = user, None
        elif server == LID_SERVER:
            phone, lid = pn_of_lid.get(user), user
        else:
            return None
        key = f"pn:{phone}" if phone else f"lid:{lid}"
        entry = merged.setdefault(key, [phone, None, []])
        if lid:
            entry[1] = lid
        return entry

    # Saved names first (full, first, business), then the name people chose
    # for themselves, then names the bridge stored.
    for their_jid, full_name, first_name, business_name, push_name in contact_rows:
        entry = entry_for(str(their_jid or ""))
        if entry is None:
            continue
        for value in (full_name, first_name, business_name):
            if value and not _looks_like_number(str(value)) and str(value) not in entry[2]:
                entry[2].append(str(value))
    for their_jid, _full, _first, _business, push_name in contact_rows:
        entry = entry_for(str(their_jid or ""))
        if entry is not None and push_name and not _looks_like_number(str(push_name)) and str(push_name) not in entry[2]:
            entry[2].append(str(push_name))
    for jid, name in chat_rows:
        entry = entry_for(str(jid or ""))
        if entry is not None and name and not _looks_like_number(str(name)) and str(name) not in entry[2]:
            entry[2].append(str(name))

    # Attach the identifier to people known only by phone number.
    lid_of_pn = {pn: lid for lid, pn in pn_of_lid.items()}
    for phone, lid, names in merged.values():
        if phone and not lid:
            lid = lid_of_pn.get(phone)
        if not names:
            continue
        person = Person(name=names[0], phone_number=phone, lid=lid, other_names=tuple(names[1:]))
        directory.people.append(person)
        if phone:
            directory.by_user[phone] = person
        if lid:
            directory.by_user[lid] = person
    return directory


def _directory() -> _Directory:
    """The contact directory, reloaded at most every 30 seconds."""
    global _directory_cache
    loaded_at, directory = _directory_cache
    now = time.monotonic()
    if directory is None or now - loaded_at > _DIRECTORY_TTL_SECONDS:
        directory = _load_directory()
        _directory_cache = (now, directory)
    return directory


def _related_jids(jid_or_number: str) -> List[str]:
    """Every address of the same person: phone-number JID and identifier JID."""
    text = (jid_or_number or "").strip()
    user = _user_part(text)
    if not user:
        return []
    if "@" in text and not text.endswith(("@" + PN_SERVER, "@" + LID_SERVER)):
        return [text]  # a group or channel has one address
    found = [text] if "@" in text else []
    person = _directory().person_for(user)
    for jid in (person.jids if person else []) + [f"{user}@{PN_SERVER}", f"{user}@{LID_SERVER}"]:
        if jid not in found:
            found.append(jid)
    return found


def get_sender_name(sender_jid: str) -> str:
    """Best display name for a sender, falling back to the raw identifier."""
    if not sender_jid:
        return "Unknown"
    person = _directory().person_for(sender_jid)
    return person.name if person else sender_jid


def _chat_display_name(jid: str, stored_name: Optional[str]) -> Optional[str]:
    """Name to show for a chat: the saved contact name when the bridge only stored a number."""
    if jid.endswith(("@" + PN_SERVER, "@" + LID_SERVER)):
        person = _directory().person_for(jid)
        if person and _looks_like_number(stored_name):
            return person.name
    return stored_name


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
        # A person's history can be split between their phone-number chat
        # and their privacy-identifier chat; read both as one conversation.
        addresses = _related_jids(chat_jid)
        where_clauses.append(f"messages.chat_jid IN ({','.join('?' * len(addresses))})")
        params.extend(addresses)
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
        chat = Chat(jid=jid, name=_chat_display_name(jid, name), last_message_time=_parse_timestamp(last_time))
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
    by_name = sort_by == "name"
    sql += " ORDER BY datetime(last_message_time) DESC, jid"
    if not by_name:
        sql += " LIMIT ? OFFSET ?"
        params.extend([limit, page * limit])
    with _connect() as conn:
        rows = conn.execute(sql, tuple(params)).fetchall()
        if by_name:
            # Sorted by the name that is shown, which may come from the address book.
            rows.sort(key=lambda row: ((_chat_display_name(row[0], row[1]) or "").lower(), row[0]))
            rows = rows[page * limit:(page + 1) * limit]
        return _rows_to_chats(conn, rows, include_last_message)


def _tokens(query: str) -> List[str]:
    text = (query or "").strip().lower()
    # A phone number typed with spaces, dashes or a plus is one search word.
    if text and all(ch.isdigit() or ch in " +-()." for ch in text) and len(_digits(text)) >= 4:
        return [_digits(text)]
    return [token for token in text.split() if token]


def _digits(text: str) -> str:
    return "".join(ch for ch in text if ch.isdigit())


def _match_rank(tokens: List[str], names: List[str], phone: str = "") -> Optional[int]:
    """How well names (and a phone number) match all search words.

    None means no match. Lower is better: 0 exact name, 1 name starts with
    the query, 2 every word starts a word of the name, 3 found anywhere.
    """
    if not tokens:
        return 3
    lowered = [name.lower() for name in names if name]
    haystack = " ".join(lowered)
    name_tokens: List[str] = []
    for token in tokens:
        digits = _digits(token)
        # "054-436..." and "+972 54 436..." both find 97254436...
        if len(digits) >= 4 and phone and digits.lstrip("0") and digits.lstrip("0") in phone:
            continue
        if token in haystack:
            name_tokens.append(token)
            continue
        return None
    if not name_tokens:
        return 1  # matched on the phone number only
    query = " ".join(name_tokens)
    if query in lowered:
        return 0
    if any(name.startswith(query) for name in lowered):
        return 1
    words = [word for name in lowered for word in name.replace("-", " ").split()]
    if all(any(word.startswith(token) for word in words) for token in name_tokens):
        return 2
    return 3


def _chat_activity(conn: sqlite3.Connection) -> Dict[str, str]:
    """chat JID -> last activity as sortable UTC text."""
    return {
        jid: (last or "")
        for jid, last in conn.execute("SELECT jid, datetime(last_message_time) FROM chats")
    }


def _search_people(conn: sqlite3.Connection, tokens: List[str], limit: int) -> List[Dict[str, Any]]:
    activity = _chat_activity(conn)
    scored = []
    for person in _directory().people:
        rank = _match_rank(tokens, [person.name, *person.other_names], person.phone_number or "")
        if rank is None:
            continue
        last = max((activity.get(jid, "") for jid in person.jids), default="")
        scored.append((rank, last, person))
    # Best match first; among equals, the person you talked to most recently.
    scored.sort(key=lambda item: item[1], reverse=True)
    scored.sort(key=lambda item: item[0])
    results = []
    for rank, last, person in scored[:limit]:
        chats = [jid for jid in person.jids if jid in activity]
        count = 0
        if chats:
            marks = ",".join("?" * len(chats))
            count = conn.execute(f"SELECT COUNT(*) FROM messages WHERE chat_jid IN ({marks})", chats).fetchone()[0]
        results.append(
            {
                "name": person.name,
                "also_known_as": list(person.other_names),
                "phone_number": person.phone_number,
                "jid": person.jid,
                "chat_jids": chats,
                "message_count": count,
                "last_message_time": (last.replace(" ", "T") + "+00:00") if last else None,
            }
        )
    return results


def _search_chats(conn: sqlite3.Connection, tokens: List[str], limit: int) -> List[Dict[str, Any]]:
    scored = []
    rows = conn.execute(
        "SELECT jid, name, datetime(last_message_time) FROM chats "
        "WHERE jid NOT LIKE '%@s.whatsapp.net' AND jid NOT LIKE '%@lid'"
    ).fetchall()
    for jid, name, last in rows:
        rank = _match_rank(tokens, [name or ""])
        if rank is None:
            continue
        scored.append((rank, last or "", jid, name))
    scored.sort(key=lambda item: item[1], reverse=True)
    scored.sort(key=lambda item: item[0])
    return [
        {
            "name": name,
            "jid": jid,
            "kind": "group" if jid.endswith("@g.us") else "channel" if jid.endswith("@newsletter") else "other",
            "last_message_time": (last.replace(" ", "T") + "+00:00") if last else None,
        }
        for rank, last, jid, name in scored[:limit]
    ]


def _snippet(content: str, tokens: List[str], width: int = 180) -> str:
    """The part of a message around the first search word."""
    text = _clean_text(content, single_line=True)
    lowered = text.lower()
    positions = [lowered.find(token) for token in tokens if lowered.find(token) >= 0]
    start = max(0, (min(positions) if positions else 0) - width // 3)
    piece = text[start:start + width]
    return ("…" if start > 0 else "") + piece + ("…" if start + width < len(text) else "")


def _resolve_chat_filter(conn: sqlite3.Connection, in_chat: str) -> List[Dict[str, str]]:
    """Turn a chat name, phone number or JID into the chats to search in."""
    text = in_chat.strip()
    if "@" in text:
        return [{"jid": jid, "name": _chat_display_name(jid, None) or jid} for jid in _related_jids(text)]
    tokens = _tokens(text)
    found: List[Dict[str, str]] = []
    people = _search_people(conn, tokens, 5)
    groups = _search_chats(conn, tokens, 5)
    # Prefer exact and prefix matches; fall back to anything that matched.
    best = min(
        [_match_rank(tokens, [p["name"], *p["also_known_as"]], p["phone_number"] or "") for p in people]
        + [_match_rank(tokens, [g["name"] or ""]) for g in groups]
        + [9]
    )
    for person in people:
        if _match_rank(tokens, [person["name"], *person["also_known_as"]], person["phone_number"] or "") == best:
            found.extend({"jid": jid, "name": person["name"]} for jid in person["chat_jids"])
    for group in groups:
        if _match_rank(tokens, [group["name"] or ""]) == best:
            found.append({"jid": group["jid"], "name": group["name"] or group["jid"]})
    return found


def _search_messages(
    conn: sqlite3.Connection,
    tokens: List[str],
    limit: int,
    chat_jids: Optional[List[str]],
    after: Optional[str],
    before: Optional[str],
    from_me: Optional[bool],
) -> List[Dict[str, Any]]:
    where: List[str] = []
    params: List[Any] = []
    for token in tokens:
        where.append("messages.content LIKE ? ESCAPE '\\'")
        params.append(f"%{_like_escape(token)}%")
    if chat_jids is not None:
        if not chat_jids:
            return []
        where.append(f"messages.chat_jid IN ({','.join('?' * len(chat_jids))})")
        params.extend(chat_jids)
    if after:
        where.append("datetime(messages.timestamp) > ?")
        params.append(_to_utc_text(after, "after"))
    if before:
        where.append("datetime(messages.timestamp) < ?")
        params.append(_to_utc_text(before, "before"))
    if from_me is not None:
        where.append("messages.is_from_me = ?")
        params.append(1 if from_me else 0)
    sql = f"SELECT {_MESSAGE_COLUMNS} FROM messages JOIN chats ON messages.chat_jid = chats.jid"
    if where:
        sql += " WHERE " + " AND ".join(where)
    sql += " ORDER BY datetime(messages.timestamp) DESC, messages.rowid DESC LIMIT ?"
    params.append(limit)
    results = []
    for row in conn.execute(sql, tuple(params)).fetchall():
        message = _row_to_message(row)
        results.append(
            {
                "timestamp": message.timestamp.isoformat(),
                "chat": _clean_text(message.chat_name, single_line=True) or message.chat_jid,
                "chat_jid": message.chat_jid,
                "from": "Me" if message.is_from_me else _clean_text(get_sender_name(message.sender), single_line=True),
                "from_me": message.is_from_me,
                "text": _snippet(message.content, tokens),
                "media_type": message.media_type,
                "message_id": message.id,
            }
        )
    return results


def search(
    query: str = "",
    scope: str = "all",
    in_chat: Optional[str] = None,
    after: Optional[str] = None,
    before: Optional[str] = None,
    from_me: Optional[bool] = None,
    limit: int = 10,
) -> Dict[str, Any]:
    """Search people, chats and messages in one call.

    All words of the query must match, in any order. People are found in the
    whole address book, by any of their names or by phone number, whether or
    not a chat with them exists. Best matches come first; among equal matches
    the most recently active.
    """
    limit = _clamp(limit, 1, 50, 10)
    scope = (scope or "all").strip().lower()
    if scope not in ("all", "people", "chats", "messages"):
        raise WhatsAppError("scope must be one of: all, people, chats, messages")
    tokens = _tokens(query)
    filtered = bool(in_chat or after or before or from_me is not None)
    if not tokens and not filtered:
        raise WhatsAppError("Give a search query, or at least one of in_chat, after, before, from_me")

    result: Dict[str, Any] = {"query": query}
    with _connect() as conn:
        chat_jids: Optional[List[str]] = None
        if in_chat:
            searched = _resolve_chat_filter(conn, in_chat)
            chat_jids = [chat["jid"] for chat in searched]
            result["searched_in"] = sorted({chat["name"] for chat in searched})
            if not searched:
                result["note"] = f"No chat or contact matches {in_chat!r}."
        # Filters only make sense for messages, so they narrow the search to messages.
        if scope in ("all", "people") and tokens and not filtered:
            result["people"] = _search_people(conn, tokens, limit)
        if scope in ("all", "chats") and tokens and not filtered:
            result["chats"] = _search_chats(conn, tokens, limit)
        if scope in ("all", "messages"):
            result["messages"] = _search_messages(conn, tokens, limit, chat_jids, after, before, from_me)
    return result


def search_contacts(query: str) -> List[Contact]:
    """Search the address book by name or phone number."""
    with _connect() as conn:
        people = _search_people(conn, _tokens(query), 50)
    return [Contact(phone_number=p["phone_number"], name=p["name"], jid=p["jid"]) for p in people]


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    """Get all chats involving the contact: their direct chat and shared groups."""
    limit = _clamp(limit, 1, MAX_LIMIT, 20)
    page = _clamp(page, 0, 1_000_000, 0)
    addresses = _related_jids(jid)
    if not addresses:
        return []
    marks = ",".join("?" * len(addresses))
    sender_sql = " OR ".join(_sender_clause("m.sender") for _ in addresses)
    sender_params = [value for address in addresses for value in _sender_params(address)]
    with _connect() as conn:
        rows = conn.execute(
            f"""
            SELECT c.jid, c.name, c.last_message_time FROM chats c
            WHERE c.jid IN ({marks})
               OR c.jid IN (SELECT DISTINCT m.chat_jid FROM messages m WHERE {sender_sql})
            ORDER BY datetime(c.last_message_time) DESC, c.jid
            LIMIT ? OFFSET ?
            """,
            (*addresses, *sender_params, limit, page * limit),
        ).fetchall()
        return _rows_to_chats(conn, rows)


def get_last_interaction(jid: str) -> Optional[str]:
    """Get most recent message involving the contact."""
    addresses = _related_jids(jid)
    if not addresses:
        return None
    marks = ",".join("?" * len(addresses))
    sender_sql = " OR ".join(_sender_clause("messages.sender") for _ in addresses)
    sender_params = [value for address in addresses for value in _sender_params(address)]
    with _connect() as conn:
        row = conn.execute(
            f"""
            SELECT {_MESSAGE_COLUMNS} FROM messages JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid IN ({marks}) OR {sender_sql}
            ORDER BY datetime(messages.timestamp) DESC, messages.rowid DESC
            LIMIT 1
            """,
            (*addresses, *sender_params),
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
    """Get the one-to-one chat with a phone number (exact match, not a partial one).

    The chat may be stored under the person's privacy identifier; the one
    with the most recent activity is returned.
    """
    addresses = [a for a in _related_jids(sender_phone_number) if a.endswith(("@" + PN_SERVER, "@" + LID_SERVER))]
    if not addresses:
        return None
    marks = ",".join("?" * len(addresses))
    with _connect() as conn:
        rows = conn.execute(
            f"""
            SELECT jid, name, last_message_time FROM chats
            WHERE jid IN ({marks})
            ORDER BY (SELECT COUNT(*) FROM messages WHERE chat_jid = chats.jid) DESC,
                     datetime(last_message_time) DESC LIMIT 1
            """,
            tuple(addresses),
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


def bridge_status() -> Dict[str, Any]:
    """State of the bridge and the local database. No message content or secrets."""
    report: Dict[str, Any] = {}
    try:
        status, data = _api_post("health", {}, timeout=10)
        if status == 200 and "logged_in" in data:
            report["bridge_running"] = True
            report.update(data)
            if not data.get("logged_in"):
                report["problem"] = "The bridge is running but not linked to WhatsApp."
                report["how_to_fix"] = "Restart the bridge and scan the QR code (WHATSAPP_QR_PAGE=1 shows it in the browser)."
        else:
            report["bridge_running"] = True
            report["problem"] = str(data.get("message") or f"The bridge answered with HTTP {status}.")
    except WhatsAppError as exc:
        report["bridge_running"] = False
        report["problem"] = str(exc)
        report["how_to_fix"] = "Start the bridge: cd whatsapp-bridge && ./whatsapp-bridge (or: go run .)"
    try:
        with _connect() as conn:
            chats, messages, latest = conn.execute(
                "SELECT (SELECT COUNT(*) FROM chats), (SELECT COUNT(*) FROM messages), "
                "(SELECT timestamp FROM messages ORDER BY datetime(timestamp) DESC LIMIT 1)"
            ).fetchone()
        newest = _parse_timestamp(latest)
        report["stored_chats"] = chats
        report["stored_messages"] = messages
        report["newest_stored_message_at"] = newest.isoformat() if newest else None
    except WhatsAppError as exc:
        report["database_problem"] = str(exc)
    report["read_only_mode"] = os.environ.get("WHATSAPP_READ_ONLY", "").strip().lower() in ("1", "true", "yes")
    return report


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
