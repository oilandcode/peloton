import pytest

from job import Job


pytestmark = [pytest.mark.default, pytest.mark.task]


def test__stop_start_all_tasks_kills_tasks_and_job():
    job = Job(job_file='long_running_job.yaml')
    job.create()
    job.wait_for_state(goal_state='RUNNING')

    job.stop()
    job.wait_for_state(goal_state='KILLED')

    job.start()
    job.wait_for_state(goal_state='RUNNING')


def test__stop_start_tasks_when_mesos_master_down_kills_tasks_when_started(
        mesos_master):
    job = Job(job_file='long_running_job.yaml')
    job.create()
    job.wait_for_state(goal_state='RUNNING')

    mesos_master.stop()
    job.stop()
    mesos_master.start()
    job.wait_for_state(goal_state='KILLED')

    mesos_master.stop()
    job.start()
    mesos_master.start()
    job.wait_for_state(goal_state='RUNNING')


def test__stop_start_tasks_when_mesos_master_down_and_jobmgr_restarts(
        mesos_master, jobmgr):
    job = Job(job_file='long_running_job.yaml')
    job.create()
    job.wait_for_state(goal_state='RUNNING')

    mesos_master.stop()
    job.stop()
    jobmgr.restart()
    mesos_master.start()
    job.wait_for_state(goal_state='KILLED')

    mesos_master.stop()
    job.start()
    jobmgr.restart()
    mesos_master.start()
    job.wait_for_state(goal_state='RUNNING')
