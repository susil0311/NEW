/*
 * Sonora Project Original (2026)
 * Chartreux Westia (github.com/koiverse)
 * Licensed Under GPL-3.0 | see git history for contributors
 * Don't remove this copyright holder!
 */


package com.susil.sonora.together

import androidx.compose.runtime.Immutable
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

const val TogetherProtocolVersion: Int = 1

@Serializable
sealed interface TogetherMessage

// ─── Client → Server ──────────────────────────────────────────────────────────
// Server expects: hello.Type == "hello"
@Serializable
@SerialName("hello")
@Immutable
data class ClientHello(
    val protocolVersion: Int,
    val sessionId: String,
    val sessionKey: String,
    val clientId: String,
    val displayName: String,
) : TogetherMessage

// Server reads incoming type from a case switch:
// "controlRequest"  →  ControlRequest
// "heartbeatPing"   →  HeartbeatPing
// "joinDecision"    →  JoinDecision
// "addTrack"        →  AddTrackRequest
// "kick" / "ban"    →  KickParticipant / BanParticipant
// "clientLeave"     →  ClientLeave  (implied by peer disconnect / explicit leave)

@Serializable
@SerialName("controlRequest")
@Immutable
data class ControlRequest(
    val sessionId: String,
    val participantId: String,
    val action: ControlAction,
) : TogetherMessage

@Serializable
@SerialName("addTrack")
@Immutable
data class AddTrackRequest(
    val sessionId: String,
    val participantId: String,
    val track: TogetherTrack,
    val mode: AddTrackMode,
) : TogetherMessage

@Serializable
@SerialName("joinDecision")
@Immutable
data class JoinDecision(
    val sessionId: String,
    val participantId: String,
    val approved: Boolean,
) : TogetherMessage

@Serializable
@SerialName("heartbeatPing")
@Immutable
data class HeartbeatPing(
    val sessionId: String,
    val pingId: Long,
    val clientElapsedRealtimeMs: Long,
) : TogetherMessage

@Serializable
@SerialName("clientLeave")
@Immutable
data class ClientLeave(
    val sessionId: String,
    val participantId: String,
) : TogetherMessage

@Serializable
@SerialName("kick")
@Immutable
data class KickParticipant(
    val sessionId: String,
    val participantId: String,
    val reason: String? = null,
) : TogetherMessage

@Serializable
@SerialName("ban")
@Immutable
data class BanParticipant(
    val sessionId: String,
    val participantId: String,
    val reason: String? = null,
) : TogetherMessage

// ─── Server → Client ──────────────────────────────────────────────────────────
// Server sends: Type: "welcome"
@Serializable
@SerialName("welcome")
@Immutable
data class ServerWelcome(
    val protocolVersion: Int,
    val sessionId: String,
    val participantId: String,
    val role: ServerRole,
    val isPending: Boolean,
    val settings: TogetherRoomSettings,
) : TogetherMessage

// Server sends: Type: "error"
@Serializable
@SerialName("error")
@Immutable
data class ServerError(
    val sessionId: String?,
    val message: String,
    val code: String? = null,
) : TogetherMessage

// Server sends: Type: "roomState"
@Serializable
@SerialName("roomState")
@Immutable
data class RoomStateMessage(
    val state: TogetherRoomState,
) : TogetherMessage

// Server sends: Type: "joinRequest"
@Serializable
@SerialName("joinRequest")
@Immutable
data class JoinRequest(
    val sessionId: String,
    val participant: TogetherParticipant,
) : TogetherMessage

// Server sends: Type: "participantJoined"
@Serializable
@SerialName("participantJoined")
@Immutable
data class ParticipantJoined(
    val sessionId: String,
    val participant: TogetherParticipant,
) : TogetherMessage

// Server sends: Type: "participantLeft"
@Serializable
@SerialName("participantLeft")
@Immutable
data class ParticipantLeft(
    val sessionId: String,
    val participantId: String,
    val reason: String? = null,
) : TogetherMessage

// Server sends: Type: "heartbeatPong"
@Serializable
@SerialName("heartbeatPong")
@Immutable
data class HeartbeatPong(
    val sessionId: String,
    val pingId: Long,
    val clientElapsedRealtimeMs: Long,
    val serverElapsedRealtimeMs: Long,
) : TogetherMessage

// ─── Enums & sealed actions ───────────────────────────────────────────────────

@Serializable
enum class ServerRole {
    HOST,
    GUEST,
}

@Serializable
enum class AddTrackMode {
    PLAY_NEXT,
    ADD_TO_QUEUE,
}

@Serializable
sealed interface ControlAction {
    @Serializable
    @SerialName("play")
    data object Play : ControlAction

    @Serializable
    @SerialName("pause")
    data object Pause : ControlAction

    @Serializable
    @SerialName("seek_to")
    data class SeekTo(
        val positionMs: Long,
    ) : ControlAction

    @Serializable
    @SerialName("skip_next")
    data object SkipNext : ControlAction

    @Serializable
    @SerialName("skip_previous")
    data object SkipPrevious : ControlAction

    @Serializable
    @SerialName("seek_to_index")
    data class SeekToIndex(
        val index: Int,
        val positionMs: Long = 0L,
    ) : ControlAction

    @Serializable
    @SerialName("seek_to_track")
    data class SeekToTrack(
        val trackId: String,
        val positionMs: Long = 0L,
    ) : ControlAction

    @Serializable
    @SerialName("set_repeat_mode")
    data class SetRepeatMode(
        val repeatMode: Int,
    ) : ControlAction

    @Serializable
    @SerialName("set_shuffle_enabled")
    data class SetShuffleEnabled(
        val shuffleEnabled: Boolean,
    ) : ControlAction
}
