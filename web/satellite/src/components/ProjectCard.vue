// Copyright (C) 2023 Storj Labs, Inc.
// See LICENSE for copying information.

<template>
    <v-card>
        <div class="h-100 d-flex flex-column justify-space-between">
            <v-card-item>
                <div class="d-flex justify-space-between">
                    <v-chip :color="item ? PROJECT_ROLE_COLORS[item.role] : 'primary'" variant="tonal" class="font-weight-bold my-2" size="small">
                        <component :is="Box" :size="12" class="mr-1" />
                        {{ item?.role || 'Project' }}
                    </v-chip>
                </div>
                <v-card-title :class="{ 'text-primary': item && item.role !== ProjectRole.Invited }">
                    <a v-if="item && item.role !== ProjectRole.Invited" class="link text-decoration-none" @click="openProject">
                        {{ item.name }}
                    </a>
                    <template v-else>
                        {{ item ? item.name : 'Welcome' }}
                    </template>
                </v-card-title>
                <v-card-subtitle v-if="!item || item.description">
                    {{ item ? item.description : 'Create a project to get started.' }}
                </v-card-subtitle>
            </v-card-item>
            <v-card-text class="flex-grow-0">
                <v-divider class="mt-1 mb-4" />
                <v-btn v-if="!item" color="primary" size="small" class="mr-2" @click="emit('createClick')">
                    Create Project
                </v-btn>
                <template v-else-if="item?.role === ProjectRole.Invited">
                    <v-btn color="primary" size="small" class="mr-2" :disabled="isDeclining" @click="emit('joinClick')">
                        Join Project
                    </v-btn>
                    <v-btn
                        variant="outlined"
                        color="default"
                        size="small"
                        class="mr-2"
                        :loading="isDeclining"
                        @click="declineInvitation"
                    >
                        Decline
                    </v-btn>
                </template>
                <v-btn v-else color="primary" size="small" rounded="md" class="mr-2" @click="openProject">Open Project</v-btn>
                <v-btn v-if="item?.role === ProjectRole.Owner" color="default" variant="outlined" size="small" rounded="md" density="comfortable" icon>
                    <v-icon :icon="Ellipsis" />

                    <v-menu activator="parent" location="bottom" transition="fade-transition">
                        <v-list class="pa-1">
                            <v-list-item link @click="() => onSettingsClick()">
                                <template #prepend>
                                    <component :is="Settings" :size="18" />
                                </template>
                                <v-list-item-title class="text-body-2 ml-3">
                                    Project Settings
                                </v-list-item-title>
                            </v-list-item>

                            <v-divider class="my-1" />

                            <v-list-item link class="mt-1" @click="emit('inviteClick')">
                                <template #prepend>
                                    <component :is="UserPlus" :size="18" />
                                </template>
                                <v-list-item-title class="text-body-2 ml-3">
                                    Add Members
                                </v-list-item-title>
                            </v-list-item>
                        </v-list>
                    </v-menu>
                </v-btn>
            </v-card-text>
        </div>
    </v-card>
</template>

<script setup lang="ts">
import { ref } from 'vue';
import { useRouter } from 'vue-router';
import {
    VCard,
    VCardItem,
    VChip,
    VBtn,
    VIcon,
    VMenu,
    VList,
    VListItem,
    VListItemTitle,
    VDivider,
    VCardTitle,
    VCardSubtitle,
    VCardText,
} from 'vuetify/components';
import { Ellipsis, Settings, UserPlus, Box } from 'lucide-vue-next';

import { ProjectItemModel, PROJECT_ROLE_COLORS, ProjectInvitationResponse } from '@/types/projects';
import { ProjectRole } from '@/types/projectMembers';
import { useProjectsStore } from '@/store/modules/projectsStore';
import { useAnalyticsStore } from '@/store/modules/analyticsStore';
import { AnalyticsErrorEventSource, AnalyticsEvent } from '@/utils/constants/analyticsEventNames';
import { useNotify } from '@/utils/hooks';
import { ROUTES } from '@/router';
import { useBucketsStore } from '@/store/modules/bucketsStore';

const props = defineProps<{
    item?: ProjectItemModel,
}>();

const emit = defineEmits<{
    joinClick: [];
    createClick: [];
    inviteClick: [];
}>();

const analyticsStore = useAnalyticsStore();
const bucketsStore = useBucketsStore();
const projectsStore = useProjectsStore();
const router = useRouter();
const notify = useNotify();

const isDeclining = ref<boolean>(false);

/**
 * Selects the project and navigates to the project dashboard.
 */
function openProject(): void {
    if (!props.item) return;

    // There is no reason to clear s3 data if the user is navigating to the previously selected project.
    if (projectsStore.state.selectedProject.id !== props.item.id) bucketsStore.clearS3Data();

    projectsStore.selectProject(props.item.id);

    router.push({
        name: ROUTES.Dashboard.name,
        params: { id: projectsStore.state.selectedProject.urlId },
    });
    analyticsStore.eventTriggered(AnalyticsEvent.NAVIGATE_PROJECTS);
}

/**
 * Selects the project and navigates to the project's settings.
 */
function onSettingsClick(): void {
    if (!props.item) return;
    projectsStore.selectProject(props.item.id);
    router.push({
        name: ROUTES.ProjectSettings.name,
        params: { id: projectsStore.state.selectedProject.urlId },
    });
}

/**
 * Declines the project invitation.
 */
async function declineInvitation(): Promise<void> {
    if (!props.item || isDeclining.value) return;
    isDeclining.value = true;

    try {
        await projectsStore.respondToInvitation(props.item.id, ProjectInvitationResponse.Decline);
        analyticsStore.eventTriggered(AnalyticsEvent.PROJECT_INVITATION_DECLINED);
    } catch (error) {
        error.message = `Failed to decline project invitation. ${error.message}`;
        notify.notifyError(error, AnalyticsErrorEventSource.PROJECT_INVITATION);
    }

    try {
        await projectsStore.getUserInvitations();
        await projectsStore.getProjects();
    } catch (error) {
        error.message = `Failed to reload projects and invitations list. ${error.message}`;
        notify.notifyError(error, AnalyticsErrorEventSource.PROJECT_INVITATION);
    }

    isDeclining.value = false;
}
</script>
