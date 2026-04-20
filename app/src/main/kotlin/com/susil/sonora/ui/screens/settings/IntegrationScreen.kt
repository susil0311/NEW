/*
 * Sonora Project Original (2026)
 * Chartreux Westia (github.com/koiverse)
 * Licensed Under GPL-3.0 | see git history for contributors
 * Don't remove this copyright holder!
 */




package com.susil.sonora.ui.screens.settings

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsetsSides
import androidx.compose.foundation.layout.only
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarScrollBehavior
import androidx.compose.runtime.Composable
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.res.stringResource
import androidx.navigation.NavController
import com.susil.sonora.LocalPlayerAwareWindowInsets
import com.susil.sonora.R
import com.susil.sonora.ui.component.PreferenceGroupTitle
import com.susil.sonora.constants.ListenBrainzEnabledKey
import com.susil.sonora.constants.ListenBrainzTokenKey
import com.susil.sonora.ui.component.IconButton
import com.susil.sonora.ui.component.InfoLabel
import com.susil.sonora.ui.component.PreferenceEntry
import com.susil.sonora.ui.component.SwitchPreference
import com.susil.sonora.ui.component.TextFieldDialog
import com.susil.sonora.ui.utils.backToMain
import com.susil.sonora.utils.rememberPreference

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun IntegrationScreen(
    navController: NavController,
    scrollBehavior: TopAppBarScrollBehavior,
) {
    val context = LocalContext.current

    val (listenBrainzEnabled, onListenBrainzEnabledChange) = rememberPreference(ListenBrainzEnabledKey, false)
    val (listenBrainzToken, onListenBrainzTokenChange) = rememberPreference(ListenBrainzTokenKey, "")

    var showListenBrainzTokenEditor = remember { mutableStateOf(false) }

    Column(
        Modifier
            .windowInsetsPadding(LocalPlayerAwareWindowInsets.current.only(WindowInsetsSides.Horizontal + WindowInsetsSides.Bottom))
            .verticalScroll(rememberScrollState())
    ) {
        Spacer(
            Modifier.windowInsetsPadding(
                LocalPlayerAwareWindowInsets.current.only(
                    WindowInsetsSides.Top
                )
            )
        )

        PreferenceGroupTitle(
                title = stringResource(R.string.general),
            )

        PreferenceEntry(
            title = { Text(stringResource(R.string.discord_integration)) },
            icon = { Icon(painterResource(R.drawable.discord), null) },
            onClick = {
                navController.navigate("settings/discord")
            },
        )

        PreferenceGroupTitle(
            title = stringResource(R.string.scrobbling),
        )

        PreferenceEntry(
            title = { Text(stringResource(R.string.lastfm_integration)) },
            icon = { Icon(painterResource(R.drawable.token), null) },
            onClick = {
                navController.navigate("settings/lastfm")
            },
        )
        SwitchPreference(
            title = { Text(stringResource(R.string.listenbrainz_scrobbling)) },
            description = stringResource(R.string.listenbrainz_scrobbling_description),
            icon = { Icon(painterResource(R.drawable.token), null) },
            checked = listenBrainzEnabled,
            onCheckedChange = onListenBrainzEnabledChange,
        )
        PreferenceEntry(
            title = { Text(if (listenBrainzToken.isBlank()) stringResource(R.string.set_listenbrainz_token) else stringResource(R.string.edit_listenbrainz_token)) },
            icon = { Icon(painterResource(R.drawable.token), null) },
            onClick = { showListenBrainzTokenEditor.value = true },
        )
    }

    TopAppBar(
        title = { Text(stringResource(R.string.integration)) },
        navigationIcon = {
            IconButton(
                onClick = navController::navigateUp,
                onLongClick = navController::backToMain
            ) {
                Icon(
                    painterResource(R.drawable.arrow_back),
                    contentDescription = null
                )
            }
        }
    )

    if (showListenBrainzTokenEditor.value) {
        TextFieldDialog(
            initialTextFieldValue = androidx.compose.ui.text.input.TextFieldValue(listenBrainzToken),
            onDone = { data ->
                onListenBrainzTokenChange(data)
                showListenBrainzTokenEditor.value = false
            },
            onDismiss = { showListenBrainzTokenEditor.value = false },
            singleLine = true,
            maxLines = 1,
            isInputValid = {
                it.isNotEmpty()
            },
            extraContent = {
                InfoLabel(text = stringResource(R.string.listenbrainz_scrobbling_description))
            }
        )
    }
}
