/*
 ============================================================================
 Name        : hev-jni.c
 Author      : hev <r@hev.cc>
 Copyright   : Copyright (c) 2019 - 2023 hev
 Description : Jave Native Interface
 ============================================================================
 */

#ifdef ANDROID

#include <jni.h>
#include <pthread.h>

#include <stdio.h>
#include <stdlib.h>
#include <signal.h>
#include <string.h>
#include <unistd.h>

#include "hev-main.h"
#include "hev-socks5-tunnel.h"

#include "hev-jni.h"

/* clang-format off */
#ifndef PKGNAME
#define PKGNAME hev/htproxy
#endif
#ifndef CLSNAME
#define CLSNAME TProxyService
#endif
/* clang-format on */

#define STR(s) STR_ARG (s)
#define STR_ARG(c) #c
#define N_ELEMENTS(arr) (sizeof (arr) / sizeof ((arr)[0]))

typedef struct _ThreadData ThreadData;

struct _ThreadData
{
    char *path;
    int fd;
};

static JavaVM *java_vm;
static int is_working;
static int worker_exited;
static pthread_t work_thread;
static pthread_mutex_t mutex;
static pthread_key_t current_jni_env;

static void native_start_service (JNIEnv *env, jobject thiz, jstring conig_path,
                                  jint fd);
static void native_stop_service (JNIEnv *env, jobject thiz);
static jboolean native_is_running (JNIEnv *env, jobject thiz);
static jlongArray native_get_stats (JNIEnv *env, jobject thiz);

static JNINativeMethod native_methods[] = {
    { "TProxyStartService", "(Ljava/lang/String;I)V",
      (void *)native_start_service },
    { "TProxyStopService", "()V", (void *)native_stop_service },
    { "TProxyIsRunning", "()Z", (void *)native_is_running },
    { "TProxyGetStats", "()[J", (void *)native_get_stats },
};

static void
throw_illegal_state (JNIEnv *env, const char *message)
{
    jclass klass;

    klass = (*env)->FindClass (env, "java/lang/IllegalStateException");
    if (!klass)
        return;
    (*env)->ThrowNew (env, klass, message);
    (*env)->DeleteLocalRef (env, klass);
}

static void
detach_current_thread (void *env)
{
    (*java_vm)->DetachCurrentThread (java_vm);
}

jint
JNI_OnLoad (JavaVM *vm, void *reserved)
{
    JNIEnv *env = NULL;
    jclass klass;

    java_vm = vm;
    if (JNI_OK != (*vm)->GetEnv (vm, (void **)&env, JNI_VERSION_1_4)) {
        return 0;
    }

    klass = (*env)->FindClass (env, STR (PKGNAME) "/" STR (CLSNAME));
    (*env)->RegisterNatives (env, klass, native_methods,
                             N_ELEMENTS (native_methods));
    (*env)->DeleteLocalRef (env, klass);

    pthread_key_create (&current_jni_env, detach_current_thread);
    pthread_mutex_init (&mutex, NULL);

    return JNI_VERSION_1_4;
}

static void *
thread_handler (void *data)
{
    ThreadData *tdata = data;

    hev_socks5_tunnel_main (tdata->path, tdata->fd);

    free (tdata->path);
    free (tdata);

    pthread_mutex_lock (&mutex);
    if (is_working && pthread_equal (work_thread, pthread_self ()))
        worker_exited = 1;
    pthread_mutex_unlock (&mutex);

    return NULL;
}

static void
native_start_service (JNIEnv *env, jobject thiz, jstring config_path, jint fd)
{
    const jbyte *bytes;
    ThreadData *tdata;
    int res;

    pthread_mutex_lock (&mutex);

    if (is_working) {
        if (worker_exited) {
            pthread_join (work_thread, NULL);
            is_working = 0;
            worker_exited = 0;
        } else {
            throw_illegal_state (env, "tun2socks is already running");
            goto exit;
        }
    }

    tdata = malloc (sizeof (ThreadData));
    if (!tdata) {
        throw_illegal_state (env, "tun2socks allocation failed");
        goto exit;
    }
    tdata->fd = fd;

    bytes = (const jbyte *)(*env)->GetStringUTFChars (env, config_path, NULL);
    if (!bytes) {
        free (tdata);
        throw_illegal_state (env, "tun2socks config path unavailable");
        goto exit;
    }
    tdata->path = strdup ((const char *)bytes);
    (*env)->ReleaseStringUTFChars (env, config_path, (const char *)bytes);
    if (!tdata->path) {
        free (tdata);
        throw_illegal_state (env, "tun2socks config path allocation failed");
        goto exit;
    }

    hev_socks5_tunnel_prepare_start ();
    is_working = 1;
    worker_exited = 0;
    res = pthread_create (&work_thread, NULL, thread_handler, tdata);
    if (res != 0) {
        is_working = 0;
        worker_exited = 0;
        free (tdata->path);
        free (tdata);
        throw_illegal_state (env, "tun2socks worker thread failed to start");
        goto exit;
    }
exit:
    pthread_mutex_unlock (&mutex);
}

static void
native_stop_service (JNIEnv *env, jobject thiz)
{
    pthread_t thread;
    int join_thread = 0;
    int stopped = 0;
    int attempts;

    pthread_mutex_lock (&mutex);

    if (!is_working)
        goto exit;

    thread = work_thread;
    if (worker_exited) {
        stopped = 1;
    } else {
        (void)hev_socks5_tunnel_quit ();
    }
    join_thread = 1;

    pthread_mutex_unlock (&mutex);

    if (join_thread) {
        for (attempts = 0; attempts < 100; attempts++) {
            pthread_mutex_lock (&mutex);
            stopped = worker_exited;
            pthread_mutex_unlock (&mutex);
            if (stopped)
                break;
            if (attempts == 80)
                hev_socks5_tunnel_force_stop ();
            usleep (50 * 1000);
        }
        if (!stopped) {
            throw_illegal_state (env,
                                 "tun2socks worker did not stop within timeout");
            return;
        }
        pthread_join (thread, NULL);
        pthread_mutex_lock (&mutex);
        if (pthread_equal (work_thread, thread)) {
            is_working = 0;
            worker_exited = 0;
        }
        pthread_mutex_unlock (&mutex);
    }
    return;
exit:
    pthread_mutex_unlock (&mutex);
}

static jboolean
native_is_running (JNIEnv *env, jobject thiz)
{
    jboolean res;

    pthread_mutex_lock (&mutex);
    res = (is_working && hev_socks5_tunnel_is_running ()) ? JNI_TRUE : JNI_FALSE;
    pthread_mutex_unlock (&mutex);

    return res;
}

static jlongArray
native_get_stats (JNIEnv *env, jobject thiz)
{
    size_t tx_packets, rx_packets, tx_bytes, rx_bytes;
    jlongArray res;
    jlong array[4];

    hev_socks5_tunnel_stats (&tx_packets, &tx_bytes, &rx_packets, &rx_bytes);
    array[0] = tx_packets;
    array[1] = tx_bytes;
    array[2] = rx_packets;
    array[3] = rx_bytes;

    res = (*env)->NewLongArray (env, 4);
    (*env)->SetLongArrayRegion (env, res, 0, 4, array);

    return res;
}

#endif /* ANDROID */
