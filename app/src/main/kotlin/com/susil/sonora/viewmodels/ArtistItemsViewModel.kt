/*
 * Sonora Project Original (2026)
 * Chartreux Westia (github.com/koiverse)
 * Licensed Under GPL-3.0 | see git history for contributors
 * Don't remove this copyright holder!
 */




package com.susil.sonora.viewmodels

import android.content.Context
import androidx.lifecycle.SavedStateHandle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.susil.sonora.innertube.YouTube
import com.susil.sonora.innertube.models.BrowseEndpoint
import com.susil.sonora.innertube.models.filterExplicit
import com.susil.sonora.innertube.models.filterVideo
import com.susil.sonora.constants.HideExplicitKey
import com.susil.sonora.constants.HideVideoKey
import com.susil.sonora.models.ItemsPage
import com.susil.sonora.utils.dataStore
import com.susil.sonora.utils.get
import com.susil.sonora.utils.reportException
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import javax.inject.Inject

@HiltViewModel
class ArtistItemsViewModel
@Inject
constructor(
    @ApplicationContext val context: Context,
    savedStateHandle: SavedStateHandle,
) : ViewModel() {
    private val browseId = savedStateHandle.get<String>("browseId")!!
    private val params = savedStateHandle.get<String>("params")

    val title = MutableStateFlow("")
    val itemsPage = MutableStateFlow<ItemsPage?>(null)

    init {
        viewModelScope.launch {
            YouTube
                .artistItems(
                    BrowseEndpoint(
                        browseId = browseId,
                        params = params,
                    ),
                ).onSuccess { artistItemsPage ->
                    title.value = artistItemsPage.title
                    itemsPage.value =
                        ItemsPage(
                            items = artistItemsPage.items
                                .distinctBy { it.id }
                                .filterExplicit(context.dataStore.get(HideExplicitKey, false))
                                .filterVideo(context.dataStore.get(HideVideoKey, false)),
                            continuation = artistItemsPage.continuation,
                        )
                }.onFailure {
                    reportException(it)
                }
        }
    }

    fun loadMore() {
        viewModelScope.launch {
            val oldItemsPage = itemsPage.value ?: return@launch
            val continuation = oldItemsPage.continuation ?: return@launch
            YouTube
                .artistItemsContinuation(continuation)
                .onSuccess { artistItemsContinuationPage ->
                    itemsPage.update {
                        ItemsPage(
                            items =
                            (oldItemsPage.items + artistItemsContinuationPage.items)
                                .distinctBy { it.id }
                                .filterExplicit(context.dataStore.get(HideExplicitKey, false)),
                            continuation = artistItemsContinuationPage.continuation,
                        )
                    }
                }.onFailure {
                    reportException(it)
                }
        }
    }
}
